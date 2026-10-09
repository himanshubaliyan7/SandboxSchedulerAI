// Package egress is the egress proxy: the only way out of a sandbox.
//
// # Topology and identity
//
// Every sandbox runs in a network namespace that has only a loopback
// interface. For each sandbox the host creates one Unix socket inside that
// sandbox's private host run directory (runtime.Spec.HostRunDir, exposed in
// the sandbox at runtime.RunDir); the in-sandbox agent bridges
// 127.0.0.1:3128 to that socket. The socket a connection arrives on is the
// sandbox's identity. There are no tokens, no source addresses and nothing
// the sandbox can forge: a sandbox can only ever speak on its own socket,
// and the policy applied is the one attached to that socket.
//
// The proxy runs on the host, outside the sandbox, as the same unprivileged
// user as the host agent. It speaks, on the same socket, selected by the
// first byte of the connection:
//
//   - HTTP forward proxy: "CONNECT host:port" tunnels and plain-HTTP
//     absolute-form requests ("GET http://host/path HTTP/1.1");
//   - SOCKS5 (RFC 1928): CONNECT command only, no authentication, domain
//     name or IP address types, so non-HTTP TCP clients work under the
//     same policy. There is no UDP and no BIND.
//
// # Threat model
//
// Everything arriving on a sandbox socket is hostile input written by
// attacker-controlled code: malformed requests, oversized or trickled
// headers, lookalike hostnames, numeric host tricks, DNS names that the
// attacker controls and can re-point at will, connection floods and
// attempts to reach the host, the host's LAN, other sandboxes' services or
// the cloud metadata service. The proxy's job is to make sure that a
// sandbox can open TCP connections only to destinations its policy
// permits, never to a non-public address, and that it cannot exhaust the
// proxy. The host user, the Config and the policies passed to Attach and
// Update are trusted. Other sandboxes are isolated from each other because
// they hold different sockets with independent state.
//
// # Decision pipeline
//
// Every CONNECT, every SOCKS5 request and every single plain-HTTP request
// (including each request on a keep-alive connection) goes through the
// same steps in this order. Deny always wins.
//
//  1. Mode. A policy with mode "none" (or an empty mode) refuses
//     everything.
//  2. Target. The host is lower-cased and one trailing dot is removed. It
//     must then be either an IP literal in canonical textual form (IPv6 in
//     brackets where the syntax requires them, no zone) or a plausible DNS
//     hostname (ASCII letters, digits, '-' and '_' in labels of 1..63
//     bytes, at most 253 bytes, last label not numeric so that forms such
//     as "127.1", "0x7f.0.0.1" or "2130706433" are never handed to a
//     resolver). IDNA is out of scope: non-ASCII names are rejected, an
//     "xn--" name is treated as the plain ASCII name it is. The port must
//     be a decimal 1..65535 without sign, spaces or leading zeros, and, if
//     Config.AllowedPorts is set, a member of it.
//  3. Allowlist (mode "allowlist" only; mode "full" skips this step). A
//     hostname must equal an AllowDomains entry or fall under a
//     "*.example.com" wildcard. Matching is by whole labels: a wildcard
//     matches any depth of subdomain and never the apex, and
//     "evilexample.com" matches neither "example.com" nor "*.example.com".
//     An IP-literal target is permitted only if it lies in AllowCIDRs.
//     AllowCIDRs does not make a hostname acceptable.
//  4. Resolution and address filter. The proxy resolves the name itself,
//     exactly once per request, with Config.Resolver (the sandbox has no
//     resolver). IPv4-mapped IPv6 results are converted to IPv4 first.
//     Every address in the deny set is dropped; if nothing is left the
//     request is refused. The deny set is the built-in set below plus the
//     policy's DenyCIDRs. AllowCIDRs never overrides it.
//  5. Dial. The proxy connects to the exact address that passed step 4 and
//     never resolves again: this is the DNS-rebinding defence. The dialer
//     has a connect timeout and a Control hook that re-checks the address
//     of the socket actually being connected against the deny set, and
//     that it is the address that was validated, as a last line of
//     defence.
//
// For tunnels (CONNECT and SOCKS5) with a hostname target, the first bytes
// the client sends are then inspected. If they are a TLS ClientHello that
// carries a server_name, the name must equal the tunnel's target host,
// otherwise the tunnel is closed with a TLS access_denied alert. Something
// that starts like a TLS handshake record but cannot be parsed as a
// ClientHello is refused. A ClientHello without SNI and traffic that is
// not TLS pass. The check is skipped for IP-literal targets (the grant was
// for the address) and can be turned off with Config.DisableSNICheck.
//
// Plain HTTP requests are forwarded to the validated address with the
// request's own Host, with hop-by-hop headers and every "Proxy-*" header
// removed. Nothing is added (no Via, no X-Forwarded-For). Redirects are
// never followed: the response is returned as is and the client's next
// request goes through the pipeline again. A request whose Host header
// disagrees with the authority of its absolute-form URI is refused, as is
// a URI with userinfo or a scheme other than http. Protocol upgrades over
// plain HTTP are not supported (Upgrade is a hop-by-hop header); use
// CONNECT.
//
// # Built-in deny set
//
// Not overridable by any policy: loopback (127.0.0.0/8, ::1), unspecified
// (0.0.0.0/8, ::), link-local (169.254.0.0/16, fe80::/10), the cloud
// metadata addresses 169.254.169.254, 100.100.100.200, 168.63.129.16 and
// fd00:ec2::254, RFC 1918 (10/8, 172.16/12, 192.168/16), CGNAT
// (100.64.0.0/10), ULA (fc00::/7), multicast (224.0.0.0/4, ff00::/8),
// broadcast and 240.0.0.0/4, the documentation, benchmark and reserved
// ranges (192.0.0.0/24, 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24,
// 192.88.99.0/24, 198.18.0.0/15, 2001:db8::/32, 3fff::/20, 2001:2::/48,
// 100::/64, fec0::/10, 64:ff9b:1::/48, the rest of 2001::/23), every IPv6
// address outside 2000::/3 other than the NAT64 well-known prefix (this
// covers the IPv4-compatible and IPv4-translated forms), NAT64
// (64:ff9b::/96), 6to4 (2002::/16) and Teredo (2001::/32) addresses that
// embed a denied IPv4 address, any address with a zone, and every address
// currently assigned to a host interface (re-read periodically).
//
// The one exception is operator configuration, never sandbox policy:
// Config.PermitPrivateCIDRs can open sub-ranges of RFC 1918, CGNAT or ULA
// space for intranet use. It is empty by default, it cannot name loopback,
// link-local, metadata or any other range, and metadata addresses and host
// interface addresses stay denied even inside a permitted range. A
// permitted range only removes the built-in denial; the per-sandbox policy
// must still allow the destination.
//
// # Limits
//
// Per sandbox: a cap on concurrent connections (excess connections get an
// HTTP 503 and are closed). Per connection: a deadline for reading the
// request header or SOCKS handshake and a cap on header size (a slow or
// oversized header is cut off), an idle timeout for tunnels and for the
// gaps between keep-alive requests, a maximum lifetime for a tunnel or an
// HTTP exchange, and fixed-size relay buffers.
//
// # What the sandbox sees
//
// Policy refusals are HTTP 403 with a one-line reason body and
// X-Egress-Verdict / X-Egress-Reason headers, or SOCKS5 reply 0x02
// (connection not allowed by ruleset). Upstream failures are 502 (resolve
// failed, connection refused or unreachable; SOCKS 0x04, 0x05, 0x03, 0x01)
// and 504 (timeout; SOCKS 0x06), so code in the sandbox can tell "blocked
// by policy" from "upstream down". Malformed requests are 400, 408 or 431.
//
// # Socket ownership and mode
//
// Attach creates the socket with Config.SocketMode, default 0600, owned by
// the user running the proxy. Connecting to a Unix socket needs write
// permission on it, so with the default mode the in-sandbox process that
// connects (the sbx-agent bridge) must run as the user-namespace UID that
// maps to the host user running the proxy: container root under the
// default rootless Podman mapping, or the same UID under
// --userns=keep-id. If the bridge runs as any other in-sandbox UID the
// caller must either set SocketMode to 0666 or arrange ownership itself.
// The caller must also ensure that the directory holding the socket
// exists, is on a path short enough for a Unix socket address (about 107
// bytes), and is not reachable by other host users or other sandboxes
// (mode 0700, one directory per sandbox): the directory, not the socket
// mode, is what keeps one sandbox off another's socket. The sandbox can
// usually unlink or replace the socket file inside its own run directory;
// that only costs it its own egress. The mode is applied without following
// symlinks, so a sandbox that swaps the path for a symlink cannot redirect
// a chmod onto a host file.
//
// # Residual risks
//
//   - Domain fronting. The proxy does not terminate TLS. The SNI check
//     only catches a client whose ClientHello openly names another host.
//     An allowlisted name served from a shared TLS endpoint (a CDN, a
//     cloud load balancer, an object store) lets the sandbox reach any
//     other tenant of that endpoint by sending the allowlisted SNI and a
//     different Host header inside the encrypted stream. Encrypted Client
//     Hello hides the inner name entirely; only the outer name is seen
//     (and a connection whose outer name differs from the target is
//     refused). A ClientHello with no SNI passes. Allowlist narrow,
//     single-purpose hostnames, not shared front ends.
//   - DNS ownership. Whoever controls an allowlisted name's DNS controls
//     where it points within public address space. The proxy pins the
//     address per connection and refuses non-public results, but it cannot
//     tell a legitimate public address from an attacker's.
//   - Exfiltration. Any data the sandbox can read can be sent to any host
//     the policy allows, in request bodies, paths, headers, or the timing
//     and choice of names it asks the proxy to resolve (each allowed
//     lookup reaches the name's authoritative servers). Mode "full" allows
//     every public destination.
//   - Host services on public addresses. Addresses assigned to host
//     interfaces are denied, but a public address that reaches the host
//     through NAT or a load balancer without being configured on an
//     interface is indistinguishable from any other public address; add
//     it to DenyCIDRs.
//   - Plain HTTP is forwarded as the client wrote it apart from the
//     header stripping described above; the upstream sees the sandbox's
//     request and the proxy host's source address.
package egress
