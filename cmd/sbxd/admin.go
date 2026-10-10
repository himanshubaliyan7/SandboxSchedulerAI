package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman"
	"github.com/himanshubaliyan7/warmbox/internal/server"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

const adminUsage = `usage: sbxd admin <resource> <action> [flags]

  tenant   create | update | list | delete
  key      create | list | revoke
  template apply | list | show | delete
  webhook  create | list | delete
  host     destroy

Every action takes --database-url (or SBX_DATABASE_URL) and --json.

examples:
  sbxd admin tenant create --id acme --max-concurrent 20 --rate 50 --burst 100
  sbxd admin key create --tenant acme            # prints the key once
  sbxd admin template apply --id python --image docker.io/library/python:3.12-alpine \
      --cpus 1 --memory 512m --pids 256 --workdir-size 128m --tmp-size 64m \
      --network allowlist --idle-timeout 10m --max-lifetime 1h --warm-target 3 --max-total 20
  sbxd admin template apply --file templates.json   # one object or an array
  sbxd admin webhook create --tenant acme --url https://hooks.example.com/warmbox \
      --events sandbox.terminated,command.finished   # prints the signing secret once
  sbxd admin host destroy --run-dir /run/user/1000/sbx   # server stopped: remove its containers,
                                                         # run dirs and live sandbox rows
`

// adminFlags are the flags every admin action shares.
type adminFlags struct {
	fs     *flag.FlagSet
	dbURL  *string
	asJSON *bool
	stdout io.Writer
}

func newAdminFlags(name string, stdout, stderr io.Writer) *adminFlags {
	fs := flag.NewFlagSet("sbxd admin "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return &adminFlags{
		fs:     fs,
		dbURL:  fs.String("database-url", envStr("SBX_DATABASE_URL", ""), "Postgres URL [SBX_DATABASE_URL]"),
		asJSON: fs.Bool("json", false, "print the result as JSON"),
		stdout: stdout,
	}
}

// open parses the flags and connects to the store (applying migrations,
// so admin commands work against an empty database).
func (a *adminFlags) open(args []string) (*store.Store, context.Context, func(), error) {
	if err := parse(a.fs, args); err != nil {
		return nil, nil, nil, err
	}
	if *a.dbURL == "" {
		return nil, nil, nil, errors.New("a database URL is required (--database-url or SBX_DATABASE_URL)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	st, err := store.Open(ctx, *a.dbURL)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return st, ctx, func() { st.Close(); cancel() }, nil
}

func (a *adminFlags) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *adminFlags) table(header string, rows func(w io.Writer)) {
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	rows(tw)
	_ = tw.Flush()
}

func cmdAdmin(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		fmt.Fprint(stderr, adminUsage)
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return nil
		}
		return errUsage
	}
	name := args[0] + " " + args[1]
	a := newAdminFlags(name, stdout, stderr)
	rest := args[2:]
	switch name {
	case "tenant create", "tenant update":
		return adminTenantWrite(a, rest, args[1] == "create")
	case "tenant list":
		return adminTenantList(a, rest)
	case "tenant delete":
		return adminTenantDelete(a, rest)
	case "key create":
		return adminKeyCreate(a, rest)
	case "key list":
		return adminKeyList(a, rest)
	case "key revoke":
		return adminKeyRevoke(a, rest)
	case "template apply":
		return adminTemplateApply(a, rest)
	case "template list":
		return adminTemplateList(a, rest)
	case "template show":
		return adminTemplateShow(a, rest)
	case "template delete":
		return adminTemplateDelete(a, rest)
	case "webhook create":
		return adminWebhookCreate(a, rest)
	case "webhook list":
		return adminWebhookList(a, rest)
	case "webhook delete":
		return adminWebhookDelete(a, rest)
	case "host destroy":
		return adminHostDestroy(a, rest)
	}
	fmt.Fprintf(stderr, "sbxd admin: unknown action %q\n\n%s", name, adminUsage)
	return errUsage
}

// --- host --------------------------------------------------------------------

// adminHostDestroy removes everything one scheduler identity owns, with
// the server stopped: its containers, its run dirs and its live rows.
func adminHostDestroy(a *adminFlags, args []string) error {
	hostID := a.fs.String("host-id", envStr("SBX_HOST_ID", ""),
		"host id whose containers and rows are destroyed [SBX_HOST_ID] (default: derived from --run-dir, as serve does)")
	runDir := a.fs.String("run-dir", envStr("SBX_RUN_DIR", ""),
		"that server's run dir: checked for a running server, and its per-sandbox directories are removed [SBX_RUN_DIR]")
	mode := a.fs.String("podman-mode", envStr("SBX_PODMAN_MODE", ""),
		"managed (start a throwaway Podman service, as the server did; the default without --podman-socket) or external [SBX_PODMAN_MODE]")
	socket := a.fs.String("podman-socket", envStr("SBX_PODMAN_SOCKET", ""), "Podman API socket of an existing service; implies external mode [SBX_PODMAN_SOCKET]")
	noDB := a.fs.Bool("no-database", false, "only remove containers and run dirs; leave the sandbox rows to the next server start")
	if err := parse(a.fs, args); err != nil {
		return err
	}
	if *hostID == "" && *runDir == "" {
		// The same defaults as serve, so that the pair "sbxd serve" /
		// "sbxd admin host destroy" works without flags.
		*runDir = server.DefaultRunDir()
	}
	if *hostID == "" && *runDir == "" {
		return errors.New("--host-id or --run-dir is required")
	}
	dbURL := *a.dbURL
	switch {
	case *noDB:
		dbURL = ""
	case dbURL == "":
		return errors.New("a database URL is required (--database-url or SBX_DATABASE_URL), or pass --no-database to leave the rows alone")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep, err := server.DestroyHost(ctx, server.DestroyHostConfig{HostID: *hostID, RunDir: *runDir, PodmanSocket: *socket, PodmanMode: *mode,
		DatabaseURL: dbURL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(rep)
	}
	fmt.Fprintf(a.stdout, "host %s: destroyed %d container(s), removed %d run dir(s), closed %d sandbox row(s)\n",
		rep.HostID, rep.Containers, rep.RunDirs, rep.Rows)
	return nil
}

// --- tenants -----------------------------------------------------------------

type tenantJSON struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	MaxConcurrent int       `json:"max_concurrent"`
	RatePerSec    float64   `json:"rate_per_sec"`
	Burst         int       `json:"burst"`
	Weight        int       `json:"weight"`
	CreatedAt     time.Time `json:"created_at"`
}

func tenantOut(t store.Tenant) tenantJSON {
	return tenantJSON{t.ID, t.Name, t.MaxConcurrent, t.RatePerSec, t.Burst, t.Weight, t.CreatedAt}
}

var idRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

func adminTenantWrite(a *adminFlags, args []string, create bool) error {
	id := a.fs.String("id", "", "tenant id (required)")
	name := a.fs.String("name", "", "display name (default: the id)")
	maxc := a.fs.Int("max-concurrent", 10, "concurrent sandbox quota")
	rps := a.fs.Float64("rate", 20, "API requests per second (0 disables rate limiting)")
	burst := a.fs.Int("burst", 40, "API request burst")
	weight := a.fs.Int("weight", 1, "scheduling weight")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if !idRE.MatchString(*id) {
		return fmt.Errorf("--id %q is invalid: use letters, digits, '.', '_' and '-' (at most 63 characters)", *id)
	}
	if *maxc < 0 || *rps < 0 || *burst < 0 || *weight < 1 {
		return errors.New("--max-concurrent, --rate and --burst must not be negative, and --weight must be at least 1")
	}
	t := store.Tenant{ID: *id, Name: *name, MaxConcurrent: *maxc, RatePerSec: *rps, Burst: *burst, Weight: *weight}
	if t.Name == "" {
		t.Name = t.ID
	}
	if create {
		t, err = st.CreateTenant(ctx, t)
	} else {
		// Flags that were not given keep the stored value.
		cur, gerr := st.GetTenant(ctx, t.ID)
		if gerr != nil {
			return gerr
		}
		set := map[string]bool{}
		a.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["name"] {
			t.Name = cur.Name
		}
		if !set["max-concurrent"] {
			t.MaxConcurrent = cur.MaxConcurrent
		}
		if !set["rate"] {
			t.RatePerSec = cur.RatePerSec
		}
		if !set["burst"] {
			t.Burst = cur.Burst
		}
		if !set["weight"] {
			t.Weight = cur.Weight
		}
		t, err = st.UpdateTenant(ctx, t)
	}
	if err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(tenantOut(t))
	}
	fmt.Fprintf(a.stdout, "tenant %s: max_concurrent=%d rate=%g/s burst=%d weight=%d\n", t.ID, t.MaxConcurrent, t.RatePerSec, t.Burst, t.Weight)
	return nil
}

func adminTenantList(a *adminFlags, args []string) error {
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	tenants, err := st.ListTenants(ctx)
	if err != nil {
		return err
	}
	if *a.asJSON {
		out := make([]tenantJSON, 0, len(tenants))
		for _, t := range tenants {
			out = append(out, tenantOut(t))
		}
		return a.printJSON(out)
	}
	a.table("ID\tNAME\tMAX_CONCURRENT\tRATE/S\tBURST\tWEIGHT", func(w io.Writer) {
		for _, t := range tenants {
			fmt.Fprintf(w, "%s\t%s\t%d\t%g\t%d\t%d\n", t.ID, t.Name, t.MaxConcurrent, t.RatePerSec, t.Burst, t.Weight)
		}
	})
	return nil
}

func adminTenantDelete(a *adminFlags, args []string) error {
	id := a.fs.String("id", "", "tenant id (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if err := st.DeleteTenant(ctx, *id); err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(map[string]string{"deleted": *id})
	}
	fmt.Fprintf(a.stdout, "tenant %s deleted (its API keys are gone; release its sandboxes separately)\n", *id)
	return nil
}

// --- API keys ----------------------------------------------------------------

type keyJSON struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// Key is the plaintext, present only in the output of "key create".
	Key string `json:"key,omitempty"`
}

func adminKeyCreate(a *adminFlags, args []string) error {
	tenant := a.fs.String("tenant", "", "tenant id (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if *tenant == "" {
		return errors.New("--tenant is required")
	}
	plaintext, key, err := st.CreateAPIKey(ctx, *tenant)
	if err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(keyJSON{ID: key.ID, TenantID: key.TenantID, Prefix: key.Prefix, CreatedAt: key.CreatedAt, Key: plaintext})
	}
	fmt.Fprintf(a.stdout, "key id:  %s\ntenant:  %s\napi key: %s\n\nThis is the only time the key is shown; only its hash is stored.\n", key.ID, key.TenantID, plaintext)
	return nil
}

func adminKeyList(a *adminFlags, args []string) error {
	tenant := a.fs.String("tenant", "", "tenant id (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if *tenant == "" {
		return errors.New("--tenant is required")
	}
	keys, err := st.ListAPIKeys(ctx, *tenant)
	if err != nil {
		return err
	}
	if *a.asJSON {
		out := make([]keyJSON, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyJSON{ID: k.ID, TenantID: k.TenantID, Prefix: k.Prefix, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt})
		}
		return a.printJSON(out)
	}
	a.table("ID\tPREFIX\tCREATED\tREVOKED", func(w io.Writer) {
		for _, k := range keys {
			revoked := "-"
			if k.RevokedAt != nil {
				revoked = k.RevokedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\tsbx_%s_…\t%s\t%s\n", k.ID, k.Prefix, k.CreatedAt.UTC().Format(time.RFC3339), revoked)
		}
	})
	return nil
}

func adminKeyRevoke(a *adminFlags, args []string) error {
	id := a.fs.String("id", "", "key id, as printed by key create or key list (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if err := st.RevokeAPIKey(ctx, *id); err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(map[string]string{"revoked": *id})
	}
	fmt.Fprintf(a.stdout, "key %s revoked (a running server may honour it for up to its --auth-cache-ttl)\n", *id)
	return nil
}

// --- templates ---------------------------------------------------------------

// templateFile is the JSON form of a template used by `template apply
// --file` and printed by `template show`. Sizes are byte counts or
// strings with a suffix ("512m"); durations are Go duration strings.
type templateFile struct {
	ID          string  `json:"id"`
	Version     int64   `json:"version,omitempty"` // output only
	Image       string  `json:"image"`
	Runtime     string  `json:"runtime,omitempty"`
	CPUs        float64 `json:"cpus"`
	Memory      size    `json:"memory"`
	PidsLimit   int64   `json:"pids_limit"`
	WorkdirSize size    `json:"workdir_size"`
	TmpSize     size    `json:"tmp_size"`
	NofileLimit int64   `json:"nofile_limit,omitempty"`
	Network     string  `json:"network,omitempty"`
	IdleTimeout dur     `json:"idle_timeout,omitempty"`
	MaxLifetime dur     `json:"max_lifetime,omitempty"`
	WarmTTL     dur     `json:"warm_ttl,omitempty"`
	WarmTarget  int     `json:"warm_target"`
	MaxTotal    int     `json:"max_total,omitempty"`
	RefillRate  int     `json:"refill_rate,omitempty"`
}

// size is a byte count that also unmarshals from "512m".
type size int64

func (s *size) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) != nil {
		str = string(b)
	}
	n, err := parseSize(str)
	*s = size(n)
	return err
}

// dur is a duration that marshals as "10m0s" and unmarshals from it.
type dur time.Duration

func (d dur) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *dur) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return errors.New("durations are strings such as \"10m\" or \"1h30m\"")
	}
	v, err := time.ParseDuration(str)
	*d = dur(v)
	return err
}

// Defaults of a template's optional fields.
const (
	defaultNofile     = 4096
	defaultRefillRate = 2
)

func (f templateFile) template() (core.Template, error) {
	t := core.Template{
		ID: f.ID, Image: f.Image, Runtime: f.Runtime,
		Resources: core.Resources{CPUs: f.CPUs, MemoryBytes: int64(f.Memory), PidsLimit: f.PidsLimit,
			WorkdirBytes: int64(f.WorkdirSize), TmpBytes: int64(f.TmpSize), NofileLimit: f.NofileLimit},
		Network:    core.NetworkMode(f.Network),
		Timeouts:   core.Timeouts{Idle: time.Duration(f.IdleTimeout), MaxLifetime: time.Duration(f.MaxLifetime), WarmTTL: time.Duration(f.WarmTTL)},
		WarmTarget: f.WarmTarget, MaxTotal: f.MaxTotal, RefillRate: f.RefillRate,
	}
	if t.Runtime == "" {
		t.Runtime = "crun"
	}
	if t.Network == "" {
		t.Network = core.NetworkNone
	}
	if t.Resources.NofileLimit == 0 {
		t.Resources.NofileLimit = defaultNofile
	}
	if t.RefillRate == 0 {
		t.RefillRate = defaultRefillRate
	}
	return t, validateTemplate(t)
}

func templateOut(t core.Template) templateFile {
	return templateFile{ID: t.ID, Version: t.Version, Image: t.Image, Runtime: t.Runtime, CPUs: t.Resources.CPUs,
		Memory: size(t.Resources.MemoryBytes), PidsLimit: t.Resources.PidsLimit, WorkdirSize: size(t.Resources.WorkdirBytes),
		TmpSize: size(t.Resources.TmpBytes), NofileLimit: t.Resources.NofileLimit, Network: string(t.Network),
		IdleTimeout: dur(t.Timeouts.Idle), MaxLifetime: dur(t.Timeouts.MaxLifetime), WarmTTL: dur(t.Timeouts.WarmTTL),
		WarmTarget: t.WarmTarget, MaxTotal: t.MaxTotal, RefillRate: t.RefillRate}
}

// validateTemplate checks what the store leaves to its callers. The
// resource rules are the Podman backend's, checked here so that a bad
// template is rejected once and not on every create.
func validateTemplate(t core.Template) error {
	r := t.Resources
	switch {
	case !idRE.MatchString(t.ID):
		return fmt.Errorf("template id %q is invalid: use letters, digits, '.', '_' and '-' (at most 63 characters)", t.ID)
	case t.Image == "":
		return errors.New("image is required")
	case r.CPUs <= 0 || r.MemoryBytes <= 0 || r.PidsLimit <= 0 || r.WorkdirBytes <= 0 || r.TmpBytes <= 0 || r.NofileLimit <= 0:
		return errors.New("cpus, memory, pids limit, workdir size, tmp size and nofile limit must all be positive")
	case r.WorkdirBytes > r.MemoryBytes-r.TmpBytes:
		return fmt.Errorf("workdir size (%d) + tmp size (%d) must fit in memory (%d): both are tmpfs and charged to the memory limit",
			r.WorkdirBytes, r.TmpBytes, r.MemoryBytes)
	case t.Network != core.NetworkNone && t.Network != core.NetworkAllowlist && t.Network != core.NetworkFull:
		return fmt.Errorf("network %q is invalid: want none, allowlist or full", t.Network)
	case t.Timeouts.Idle < 0 || t.Timeouts.MaxLifetime < 0 || t.Timeouts.WarmTTL < 0:
		return errors.New("timeouts must not be negative")
	case t.WarmTarget < 0 || t.MaxTotal < 0 || t.RefillRate < 1:
		return errors.New("warm target and max total must not be negative, and refill rate must be at least 1")
	case t.MaxTotal > 0 && t.WarmTarget > t.MaxTotal:
		return fmt.Errorf("warm target (%d) exceeds max total (%d)", t.WarmTarget, t.MaxTotal)
	}
	return nil
}

func adminTemplateApply(a *adminFlags, args []string) error {
	var f templateFile
	var mem, workdir, tmp string
	var idle, life, warmTTL time.Duration
	file := a.fs.String("file", "", "JSON file with one template object or an array of them (\"-\" for stdin); replaces the flags below")
	a.fs.StringVar(&f.ID, "id", "", "template id")
	a.fs.StringVar(&f.Image, "image", "", "container image; it must already be pulled for the Podman user")
	a.fs.StringVar(&f.Runtime, "runtime", "crun", "OCI runtime (isolation tier): crun, or runsc for gVisor; it must be among --runtimes")
	var runtimes string
	addRuntimeFlags(a.fs, &runtimes)
	a.fs.Float64Var(&f.CPUs, "cpus", 1, "CPU limit")
	a.fs.StringVar(&mem, "memory", "512m", "memory limit")
	a.fs.Int64Var(&f.PidsLimit, "pids", 256, "pids limit")
	a.fs.StringVar(&workdir, "workdir-size", "128m", "size of the /workspace tmpfs")
	a.fs.StringVar(&tmp, "tmp-size", "64m", "size of the /tmp tmpfs")
	a.fs.Int64Var(&f.NofileLimit, "nofile", defaultNofile, "open file limit")
	a.fs.StringVar(&f.Network, "network", "none", "widest network mode a sandbox may ask for: none, allowlist or full")
	a.fs.DurationVar(&idle, "idle-timeout", 10*time.Minute, "terminate a sandbox after this long without activity (0 disables)")
	a.fs.DurationVar(&life, "max-lifetime", time.Hour, "hard cap on a sandbox's lifetime (0 disables)")
	a.fs.DurationVar(&warmTTL, "warm-ttl", 30*time.Minute, "recycle a warm sandbox that was never claimed after this long (0 disables)")
	a.fs.IntVar(&f.WarmTarget, "warm-target", 2, "warm pool size")
	a.fs.IntVar(&f.MaxTotal, "max-total", 0, "cap on all sandboxes of this template (0 for none)")
	a.fs.IntVar(&f.RefillRate, "refill-rate", defaultRefillRate, "concurrent creates")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()

	var templates []core.Template
	if *file != "" {
		files, err := readTemplateFile(*file)
		if err != nil {
			return err
		}
		for _, tf := range files {
			t, err := tf.template()
			if err != nil {
				return fmt.Errorf("template %q: %w", tf.ID, err)
			}
			templates = append(templates, t)
		}
	} else {
		for _, s := range []struct {
			flag string
			in   string
			out  *size
		}{{"--memory", mem, &f.Memory}, {"--workdir-size", workdir, &f.WorkdirSize}, {"--tmp-size", tmp, &f.TmpSize}} {
			n, err := parseSize(s.in)
			if err != nil {
				return fmt.Errorf("%s: %w", s.flag, err)
			}
			*s.out = size(n)
		}
		f.IdleTimeout, f.MaxLifetime, f.WarmTTL = dur(idle), dur(life), dur(warmTTL)
		t, err := f.template()
		if err != nil {
			return err
		}
		templates = []core.Template{t}
	}

	// The store does not know what a server has enabled, so the operator
	// says it here as they say it to the server (the same flag, the same
	// variable): a template for a tier that is not enabled is refused now
	// and not when its first sandbox is created.
	enabled, err := podman.ParseRuntimes(runtimes)
	if err != nil {
		return fmt.Errorf("--runtimes: %w", err)
	}
	if len(enabled) == 0 {
		enabled = server.DefaultRuntimes()
	}
	if err := server.CheckTemplateRuntimes(templates, podman.RuntimeNames(enabled)); err != nil {
		return fmt.Errorf("%w (give `template apply` the same --runtimes / SBX_RUNTIMES as the server)", err)
	}

	var out []templateFile
	for _, t := range templates {
		saved, err := st.UpdateTemplate(ctx, t)
		verb := "updated"
		if errors.Is(err, store.ErrNotFound) {
			saved, err = st.CreateTemplate(ctx, t)
			verb = "created"
		}
		if err != nil {
			return fmt.Errorf("template %s: %w", t.ID, err)
		}
		out = append(out, templateOut(saved))
		if !*a.asJSON {
			fmt.Fprintf(a.stdout, "template %s %s (version %d, warm target %d)\n", saved.ID, verb, saved.Version, saved.WarmTarget)
		}
	}
	if *a.asJSON {
		return a.printJSON(out)
	}
	return nil
}

func readTemplateFile(path string) ([]templateFile, error) {
	var (
		b   []byte
		err error
	)
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	decode := func(v any) error {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		return dec.Decode(v)
	}
	if len(b) > 0 && b[0] == '[' {
		var many []templateFile
		if err := decode(&many); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return many, nil
	}
	var one templateFile
	if err := decode(&one); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return []templateFile{one}, nil
}

func adminTemplateList(a *adminFlags, args []string) error {
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	templates, err := st.ListTemplates(ctx)
	if err != nil {
		return err
	}
	if *a.asJSON {
		out := make([]templateFile, 0, len(templates))
		for _, t := range templates {
			out = append(out, templateOut(t))
		}
		return a.printJSON(out)
	}
	a.table("ID\tVERSION\tIMAGE\tCPUS\tMEMORY\tPIDS\tNETWORK\tIDLE\tMAX_LIFETIME\tWARM\tMAX", func(w io.Writer) {
		for _, t := range templates {
			fmt.Fprintf(w, "%s\t%d\t%s\t%g\t%s\t%d\t%s\t%s\t%s\t%d\t%d\n", t.ID, t.Version, t.Image, t.Resources.CPUs,
				humanSize(t.Resources.MemoryBytes), t.Resources.PidsLimit, t.Network, t.Timeouts.Idle, t.Timeouts.MaxLifetime, t.WarmTarget, t.MaxTotal)
		}
	})
	return nil
}

func adminTemplateShow(a *adminFlags, args []string) error {
	id := a.fs.String("id", "", "template id (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	t, err := st.GetTemplate(ctx, *id)
	if err != nil {
		return err
	}
	return a.printJSON(templateOut(t)) // the same shape `template apply --file` reads
}

func adminTemplateDelete(a *adminFlags, args []string) error {
	id := a.fs.String("id", "", "template id (required)")
	st, ctx, done, err := a.open(args)
	if err != nil {
		return err
	}
	defer done()
	if err := st.DeleteTemplate(ctx, *id); err != nil {
		return err
	}
	if *a.asJSON {
		return a.printJSON(map[string]string{"deleted": *id})
	}
	fmt.Fprintf(a.stdout, "template %s deleted; a running server recycles its warm sandboxes, sessions keep running\n", *id)
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dg", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dm", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dk", n>>10)
	}
	return fmt.Sprintf("%d", n)
}
