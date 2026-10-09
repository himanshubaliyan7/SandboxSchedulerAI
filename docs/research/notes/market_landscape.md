# Market and open-source landscape for AI agent code-execution sandboxes (as of 9 October 2026)

Reading notes for the report writer:
- All GitHub figures (stars, license, last push, archived flag) were pulled from the GitHub REST API on 2026-10-09 and are cited to the repo URL.
- Most pricing and cold-start comparisons available on the open web are vendor-authored (Northflank, Upstash, Modal, Beam, Blaxel, Temps, bex.co, Fly). Each is flagged where used. Cold-start numbers are vendor claims that measure different things and are not comparable.
- Where I could not open a primary source, the finding is attributed to the secondary source that carried it and dated.

## 1. Commercial providers: isolation, cold start, session limits, pricing, self-hosting/BYOC, funding, customers

### Takeaway
The hosted category is crowded (15+ funded providers), converged on microVMs (Firecracker / Cloud Hypervisor / custom KVM) or gVisor, and converged on price around $0.05 per vCPU-hour and $0.016 per GiB-hour. No commercial provider sells rootless containers as its isolation boundary, and self-hosting/BYOC is almost always gated behind enterprise contracts.

### Cited Findings

Pricing, limits and isolation (primary where marked, otherwise from a vendor-authored 15-provider comparison dated 17 Sep 2026 by Upstash, which sells Upstash Box and ranks it first):

- E2B (primary, pricing page fetched 2026-10-09): $0.000014 per vCPU-second (about $0.0504/vCPU-hour), $0.0000045 per GiB-second (about $0.0162/GiB-hour), storage included; default sandbox 2 vCPU / 4 GiB, max 8 vCPU / 8 GiB, CPU-only (no GPU). Hobby: $0/month, one-time $100 credit, 20 concurrent sandboxes, 1-hour sessions. Pro: $150/month plus usage, 100 concurrent, 24-hour sessions. Concurrency add-ons: 600 concurrent for $650/month, 1,100 for $1,150/month. Enterprise: $3,000/month minimum, and BYOC is an Enterprise item — [E2B pricing](https://e2b.dev/pricing)
- E2B BYOC is available on AWS and GCP only, Azure in progress, enterprise customers only — [E2B BYOC docs](https://e2b.dev/docs/byoc)
- E2B isolation: Firecracker microVMs; about 150 ms cold start (vendor case study figure); pause preserves filesystem, memory and processes — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Daytona: containers by default with a VM option; "under 90 ms" claim; $0.0504/vCPU-hour and $0.0162/GiB-hour (identical to E2B); GPU available; BYOC offered; open-source repo unmaintained since June 2026 — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Daytona defaults: auto-stop after 15 minutes of inactivity, configurable to indefinite — [Fly.io comparison page, vendor](https://fly.io/learn/fly-vs-daytona.md)
- Modal Sandboxes: gVisor (VM option); "sub-second"; 24-hour max; no in-place pause, snapshots instead; about $0.142 per physical core-hour (1 core = 2 vCPU) and about $0.024/GiB-hour; GPU available; managed only — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Modal sandbox default lifetime is 5 minutes, configurable up to 24 hours — [Modal vs Fly comparison, vendor](https://modal.com/compare/modal-vs-flyio)
- Vercel Sandbox: Firecracker microVM; 45 minutes on Hobby, 24 hours on Pro per session; $0.128 per active vCPU-hour and $0.0212/GB-hour (active-CPU billing); managed only — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026). An older table (Jan 2026) listed a 5-hour Pro cap — [Superagent benchmark, Jan 2026](https://www.superagent.sh/blog/ai-code-sandbox-benchmark-2026). Vercel Sandbox reached GA at the end of January 2026 — [bex.co, 11 Sep 2026, vendor blog](https://bex.co/blog/2026/09/11/ai-sandbox-funding-modal-daytona-e2b)
- Cloudflare Sandbox: container inside its own VM; 1–3 s cold start; no session cap but sleeps after 10 minutes idle; no persistence by default (directory backups to R2); $0.072/vCPU-hour, $0.009/GiB-hour; managed only — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Runloop Devboxes: VM on a custom hypervisor; "a few seconds" start; 1-hour default session, configurable; $0.108/CPU-hour, $0.0252/GB-hour; enterprise deploy-to-VPC — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Blaxel: microVM; about 25 ms resume from standby; memory-based billing at $0.0414/GB-hour; bring-your-own-metal with managed control plane; CPU-only — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Fly.io Sprites (launched January 2026): Firecracker microVM; 1–2 s create, 100–500 ms warm wake; no session cap; 100 GB persistent filesystem; $0.07/CPU-hour and $0.04375/GB-hour; idle storage $0.02/GB-month; managed only; CPU-only — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026); [bex.co, 25 Sep 2026](https://bex.co/blog/2026/09/25/four-companies-one-sandbox-market-agent-code-execution)
- Northflank: Kata + Cloud Hypervisor or gVisor; "under 1 second"; no cap; $0.01667/vCPU-hour and $0.00833/GB-hour; self-serve BYOC on major clouds and bring-your-own Kubernetes; GPU — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Beam: gVisor + runc; 1–3 s; no cap; $0.135 per physical core-hour, $0.023/GiB-hour; BYOC and self-host through Beta9 (AGPL-3.0); GPU — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Tensorlake: Firecracker / Cloud Hypervisor; "a few hundred ms"; 2 h free, 24 h on credits, unlimited on Pro; $0.07/core-hour (credits) or $0.042 (Pro); BYOC on AWS, GCP, Azure, CoreWeave, Nebius, bare metal — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Freestyle: KVM Linux VM; about 65 ms provision claim; $0.04032/vCPU-hour, $0.0129/GiB-hour; managed only — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026). Freestyle's founders said on HN that they moved to their own racks and that sandboxes start in about 500 ms, with full-memory VM forking in 320–400 ms — [Launch HN: Freestyle (April 2026)](https://hn.svelte.dev/item/47663147)
- Koyeb: microVM on bare metal; under 250 ms; 24-hour max with auto-delete; ephemeral; 4 vCPU / 4 GB at $0.0576/hour bundled; GPU — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Namespace: microVM on own bare metal; 0.8 s to agent connected; 3 h / 5 h / 24 h by plan — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Upstash Box: hardened container (the only listed provider using a plain container as the boundary); about 2.5 s create; memory not billed — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Morph Cloud: Firecracker microVMs, from $40/month, $0.05 per "MCU", managed only — [Temps blog, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026)
- CodeSandbox SDK / Together: microVM, about 500 ms snapshot start, up to 64 vCPU — [wincent gist "List of coding agent sandboxes 2026-05"](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)
- Riza: WebAssembly-based runtime for Python and JavaScript, claims under 10 ms start; seed-stage with $2.7M led by Matrix Partners — [Riza funding post](https://riza.io/blog/riza-raises-2-7M-to-run-ai-generated-code)

Independent and semi-independent performance data:
- Third-party measurements contradict vendor cold-start claims: E2B about 0.5 s end-to-end (fastest in a five-way test) versus a 150 ms claim; Daytona about 0.75 s measured and 2–5 s reported by a practitioner in production versus a sub-90 ms claim; Modal 56–65 s for full bootstrap in harness tests versus "sub-second" — [bex.co, 25 Sep 2026, secondary roundup](https://bex.co/blog/2026/09/25/four-companies-one-sandbox-market-agent-code-execution)
- One benchmark found E2B fastest and most consistent at 717 ms create and 662 ms resume — reported in search summary of [LogRocket comparison](https://blog.logrocket.com/comparing-ai-agent-sandbox-platforms-e2b-modal-daytona-and-more/)
- A ComputeSDK time-to-interactive benchmark under load put E2B at 1.61 s median / 1.81 s p99, Vercel Sandbox at 0.67 s / 1.12 s, Modal at 0.88 s / 1.08 s — [bex.co, 6 Sep 2026, citing a secondary roundup](https://bex.co/blog/2026/09/06/e2b-scaling-cracks-idle-costs-suspend-latency)
- An August 2026 reference-workload cost benchmark: Vercel $16.27, E2B and Daytona $27.60, Modal $39.66, a spread attributed largely to idle-billing policy — [bex.co, 25 Sep 2026](https://bex.co/blog/2026/09/25/four-companies-one-sandbox-market-agent-code-execution)

Funding and traction:
- Modal: $355M Series C in May 2026 at a $4.65B valuation (Redpoint, General Catalyst; Accel, Menlo), up from $1.1B at its Series B about eight months earlier; named customers Runway, Suno, Lovable, Quora — [bex.co, 11 Sep 2026](https://bex.co/blog/2026/09/11/ai-sandbox-funding-modal-daytona-e2b). A second source gives Modal over $466M raised in total and an $87M Series B at $1.1B post-money in September 2025 — [Runtimewire](https://runtimewire.com/article/devin-multi-vm-cloud-agents-modal-outposts). The two sources differ on Series B size ($80M versus $87M).
- Daytona: $24M Series A in February 2026 led by FirstMark with Pace Capital, Upfront, E2VC, Darkmode and strategic money from Datadog and Figma Ventures; about $31M total; $1M forward revenue run rate in under three months, doubled six weeks later; customers LangChain, Turing, Writer, SambaNova; 20-person team; "hardware-constrained" — [tech.eu, 6 Feb 2026](https://tech.eu/2026/02/06/daytona-raises-24m-series-a-to-build-agent-native-compute-infrastructure/); [AlleyWatch, Feb 2026](https://alleywatch.com/2026/02/daytona-ai-agent-infrastructure-sandbox-computing-developer-tools-ivan-burazin/)
- E2B: $21M Series A in July 2025 led by Insight Partners; no later priced round found; company claims rose from "88% of Fortune 100 signed up" to "94% of Fortune 100, 3M+ monthly SDK downloads, 1B+ started sandboxes" — [Insight Partners](https://www.insightpartners.com/ideas/e2b-raises-a-21m-series-a-to-offer-cloud-for-ai-agents-to-fortune-100/); [bex.co, 11 Sep 2026](https://bex.co/blog/2026/09/11/ai-sandbox-funding-modal-daytona-e2b). Total funding is reported as about $32M or $35M depending on source — [CB Insights](https://www.cbinsights.com/company/e2b/financials)
- E2B sandbox volume grew from about 40,000 per month in March 2024 to about 15 million per month a year later — [bex.co, 25 Sep 2026](https://bex.co/blog/2026/09/25/four-companies-one-sandbox-market-agent-code-execution)
- Runloop: $7M seed in July 2025 led by The General Partnership — [Benzatine](https://benzatine.com/news-room/runloop-secures-7-million-to-enhance-ai-development-with-cloud-integrated-environments)
- Northflank: $24.9M total including a $16M Series A led by Bain Capital Ventures in November 2024; 80k+ developers as of June 2026 — [Ry Walker research note](https://rywalker.com/research/northflank)
- Blaxel was acquired by Baseten (reported September 2026, terms undisclosed) — [The Next Web](https://thenextweb.com/news/baseten-acquires-blaxel-ai-agent-sandboxes)
- Koyeb was acquired by Mistral AI (announced February 2026, terms undisclosed) and folded into Mistral Compute — [InfoWorld](https://www.infoworld.com/article/4133757/mistral-ai-deepens-compute-ambitions-with-koyeb-acquisition.html)
- CodeSandbox was acquired by Together AI in December 2024 — [Together AI](https://www.together.ai/blog/codesandbox-acquisition-together-code-interpreter)

### Inferences
- Price is commoditised: E2B and Daytona publish identical rates, and Freestyle, Tensorlake and Northflank undercut them. A new entrant cannot win on per-second price as a hosted service without owning hardware (Freestyle, Namespace, Koyeb and Daytona all point to hardware as the constraint or the moat).
- Every funded provider positions on microVM or gVisor isolation. A product whose headline is "rootless Podman" would be the weakest stated isolation tier in the commercial field; Upstash Box is the only commercial example of a "hardened container" boundary found.
- Self-hosting is the least-served axis commercially: E2B BYOC needs a $3,000/month enterprise minimum, Runloop VPC deploy is enterprise, Modal/Vercel/Cloudflare/Sprites/Freestyle are managed only. Northflank, Tensorlake and Beam are the exceptions and all assume Kubernetes or a cloud account.
- Billing model (wall-clock versus active CPU) is now a bigger differentiator than the rate card.

### Gaps
- I did not open the primary pricing pages for Daytona, Modal, Vercel, Cloudflare, Runloop, Blaxel, Sprites or Northflank; their figures come from one vendor-authored table (Upstash, 17 Sep 2026) and should be re-verified before publication.
- No funding figures found for Morph Cloud, Freestyle, Beam, Tensorlake or Namespace. Search results for "Morph" funding were for unrelated companies.
- No revenue figures found for E2B or Modal. No current Daytona ARR beyond the February 2026 run-rate statement.
- Post-acquisition pricing and roadmap for Blaxel (Baseten) and Koyeb (Mistral) not verified.

## 2. Open-source projects: license, maturity, activity, isolation, and whether a rootless Podman warm pool already exists

### Takeaway
No mature open-source project implements a pre-warmed pool of rootless Podman sandboxes with a scheduler; what exists is either KVM-based (E2B runtime, microsandbox, Arrakis, BoxLite, Gondolin), Kubernetes-based (kubernetes-sigs/agent-sandbox with a SandboxWarmPool CRD, OpenSandbox, OpenKruise agents), Docker-socket-based (OpenHands, llm-sandbox, AIO Sandbox), or single-user Podman wrapper scripts. Daytona's open-source repo was archived in mid-2026, which leaves a visible hole for a maintained, self-hostable, container-based option.

### Cited Findings

Repository facts (GitHub API, 2026-10-09):
- daytonaio/daytona: 71,593 stars, archived, last push 2026-07-24. README states "This repository is no longer maintained. As of June 2026, Daytona's core development has moved to a private codebase. This repository will receive no further updates, fixes, or releases," remaining usable under the license at tag v0.190.0 — [daytonaio/daytona](https://github.com/daytonaio/daytona). A vendor blog dates the move to 11 June 2026 and describes the license as AGPL-3.0 — [Temps blog, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026)
- e2b-dev/infra has been renamed e2b-dev/runtime: 1,689 stars, Apache-2.0, pushed 2026-10-09, 212 open issues, described as "The runtime behind every E2B stack: Cloud, Enterprise, and your own machine" — [e2b-dev/runtime](https://github.com/e2b-dev/runtime). The SDK repo e2b-dev/E2B has 14,250 stars, Apache-2.0 — [e2b-dev/E2B](https://github.com/e2b-dev/E2B)
- Self-hosting E2B requires provisioning a cluster with Packer, Terraform and Nomad on GCP or AWS — [Temps blog, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026); [Northflank blog, 16 Feb 2026, vendor](https://northflank.com/blog/self-hostable-alternatives-to-e2b-for-ai-agents)
- kubernetes-sigs/agent-sandbox: 4,200 stars, Apache-2.0, created 2025-08-12, pushed 2026-10-09, 183 open issues — [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox). It provides Sandbox, SandboxTemplate, SandboxClaim and SandboxWarmPool resources; the warm pool "manages a pool of pre-warmed Sandboxes that can be quickly allocated" — [agent-sandbox docs](https://agent-sandbox.sigs.k8s.io/docs/getting_started/overview/). Isolation is delegated to gVisor or Kata via RuntimeClass, and the warm pool reduces cold start to "less than one second" — [Google Open Source blog, Nov 2025](https://opensource.googleblog.com/2025/11/unleashing-autonomous-ai-agents-why-kubernetes-needs-a-new-standard-for-agent-execution.html); [Kubernetes blog, 20 Mar 2026](https://kubernetes.io/blog/2026/03/20/running-agents-on-kubernetes-with-agent-sandbox)
- Google's GKE Agent Sandbox (gVisor-based) was reported GA in September 2026 with a "300 sandboxes a second" headline — [shattered.io (secondary)](https://shattered.io/gke-agent-sandbox-gvisor-300-per-second-2026/)
- superradcompany/microsandbox (formerly zerocore-ai / microsandbox org): 8,618 stars, Apache-2.0, pushed 2026-10-09; libkrun microVMs — [microsandbox](https://github.com/superradcompany/microsandbox). Its self-hosting doc says KVM must be enabled on Linux — [microsandbox SELF_HOSTING (mirror)](https://glama.ai/mcp/servers/@microsandbox/microsandbox/blob/ba02ae334e319faefd8c18dde34bce9822ab3df5/SELF_HOSTING.md). Northflank notes its developers label it experimental — [Northflank blog, 16 Feb 2026, vendor](https://northflank.com/blog/self-hostable-alternatives-to-e2b-for-ai-agents)
- abshkbh/arrakis: 885 stars, AGPL-3.0, last push 2025-06-02 (stale for 16 months) — [abshkbh/arrakis](https://github.com/abshkbh/arrakis)
- OpenHands/OpenHands (formerly All-Hands-AI): 90,344 stars, MIT, pushed 2026-10-09 — [OpenHands](https://github.com/OpenHands/OpenHands)
- anthropics/sandbox-runtime (formerly anthropic-experimental): 5,481 stars, Apache-2.0, created 2025-10-20, pushed 2026-10-09, 274 open issues; OS-level filesystem and network restriction without a container — [anthropics/sandbox-runtime](https://github.com/anthropics/sandbox-runtime). It uses Seatbelt on macOS and bubblewrap on Linux with a network proxy — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)
- opensandbox-group/OpenSandbox (formerly alibaba/OpenSandbox): 15,750 stars, Apache-2.0, created 2025-12-17, pushed 2026-10-09. Runtimes are Docker (local) and Kubernetes, with a Firecracker-based "Fast Sandbox runtime" on Kubernetes; per-sandbox egress policies; "resource pools and batch creation." The README does not mention Podman, rootless operation, gVisor or Kata — [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
- TencentCloud/CubeSandbox: 12,847 stars, created 2026-04-10, pushed 2026-10-09 ("Instant, Concurrent, Secure & Lightweight Sandbox for AI Agents") — [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
- NVIDIA/OpenShell: 15,537 stars, Apache-2.0, created 2026-02-24, pushed 2026-10-09 — [NVIDIA/OpenShell](https://github.com/NVIDIA/OpenShell)
- agent-infra/sandbox (AIO Sandbox, Docker image with shell, browser, Jupyter, VS Code, MCP): 6,102 stars, Apache-2.0 — [agent-infra/sandbox](https://github.com/agent-infra/sandbox)
- boxlite-ai/boxlite (embeddable VM): 2,526 stars, Apache-2.0, created 2025-12-07 — [boxlite](https://github.com/boxlite-ai/boxlite)
- earendil-works/gondolin (experimental Linux microVM with TypeScript control plane): 2,261 stars, Apache-2.0, created 2026-02-03 — [gondolin](https://github.com/earendil-works/gondolin)
- beam-cloud/beta9: 1,808 stars, AGPL-3.0, pushed 2026-10-09 — [beta9](https://github.com/beam-cloud/beta9)
- dagger/container-use: 4,063 stars, Apache-2.0 — [container-use](https://github.com/dagger/container-use)
- openkruise/agents: 297 stars, Apache-2.0, created 2025-11-26 — [openkruise/agents](https://github.com/openkruise/agents)
- vndee/llm-sandbox: 1,129 stars, MIT, pushed 2026-10-09 — [llm-sandbox](https://github.com/vndee/llm-sandbox)
- judge0/judge0: 4,464 stars, GPL-3.0, pushed 2026-10-08 — [judge0](https://github.com/judge0/judge0)
- engineer-man/piston: 2,828 stars, MIT, pushed 2026-07-31 — [piston](https://github.com/engineer-man/piston)
- gotempsh/temps: 828 stars, Apache-2.0; single binary offering Docker containers or Firecracker microVMs per sandbox, falling back to Docker when /dev/kvm is absent — [Temps blog, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026); [gotempsh/temps](https://github.com/gotempsh/temps)
- Building blocks: podman 33,019 stars Apache-2.0; firecracker 37,255 Apache-2.0; gVisor 19,611 Apache-2.0; kata-containers 8,906 Apache-2.0; libkrun 2,761 Apache-2.0 — [podman](https://github.com/podman-container-tools/podman); [firecracker](https://github.com/firecracker-microvm/firecracker); [gvisor](https://github.com/google/gvisor); [kata-containers](https://github.com/kata-containers/kata-containers); [libkrun](https://github.com/libkrun/libkrun)

Rootless Podman specifically:
- A web search for rootless-Podman agent sandboxes returned only small single-user wrapper projects (agentsbox, sandboxed-ai-agents, another-ai-sandbox, MiGoller/ai-agent-sandbox, ca-mantis-shrimp/agent-sandbox, aaannz/ai-agents-sandbox); none describes a pre-warmed pool or multi-tenant scheduler — [search result example: grauzone-git/sandboxed-ai-agents](https://github.com/grauzone-git/sandboxed-ai-agents); [amedeos/another-ai-sandbox](https://github.com/amedeos/another-ai-sandbox)
- jsdelfino/sandbox offers "Firecracker or Podman backends" for agent dev sandboxes but has 0 stars (created 2026-08-14) — [jsdelfino/sandbox](https://github.com/jsdelfino/sandbox)
- agentic-sandbox is a self-hostable runtime for persistent coding agents using "KVM-isolated VMs (or rootless containers)" with no hosted control plane — [jmagly/agentic-sandbox](https://github.com/jmagly/agentic-sandbox)
- The May 2026 community catalogue of 100+ coding-agent sandboxes lists Podman only as a base runtime and as the backend of one wrapper (codex-container-sandbox); it never uses the term "warm pool" — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)
- Google Gemini CLI can use Docker or Podman as its local sandbox — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)

### Inferences
- The exact combination in the idea (rootless Podman + warm pool + cgroup quotas + egress lock + recycling + WebSocket TTY, as one single-node-friendly scheduler) appears unoccupied in open source as of October 2026. The absence is of a product, not of the parts: every component exists upstream.
- The closest functional substitute is kubernetes-sigs/agent-sandbox (warm pool, claims, templates, gVisor/Kata). It is Apache-2.0, backed by Google/SIG Apps, and already productised as GKE Agent Sandbox. Anyone already on Kubernetes will default to it. The open space is therefore "no Kubernetes required", not "warm pool" as such.
- Daytona's archive (71.6k stars, the most-starred project in the category after OpenHands) strands self-hosters on a frozen AGPL codebase. That is the most concrete, datable adoption opening found in this research.
- Chinese cloud vendors (Alibaba OpenSandbox 15.7k stars, Tencent CubeSandbox 12.8k stars) and NVIDIA (OpenShell 15.5k stars) each shipped Apache-licensed sandbox runtimes in under a year. Star velocity in this category is high and attention is fragmented, so a new project cannot rely on novelty alone.
- Reusable rather than buildable: Podman (rootless, cgroups v2, pods, pasta/slirp networking), gVisor runsc as an optional OCI runtime that also needs no KVM, Firecracker/libkrun for a KVM tier, bubblewrap and anthropics/sandbox-runtime for in-sandbox process restriction, and the E2B SDK surface as a de facto API shape.

### Gaps
- I did not read source code to confirm whether OpenSandbox's "resource pools" are pre-warmed instances, or whether CubeSandbox/OpenShell support rootless or KVM-less operation. CubeSandbox license returned NOASSERTION from the API.
- The GitHub API returned no SPDX license for the archived Daytona repo; AGPL-3.0 comes from secondary sources and should be confirmed in the LICENSE file at tag v0.190.0.
- Contributor counts and release cadence were not collected for any project.
- There may be closed or unindexed rootless-pool implementations inside companies; absence of search results is not proof of absence.

## 3. What the named target companies and frontier labs use today

### Takeaway
Every named target either runs its own microVM/gVisor infrastructure or buys from a well-funded vendor at very large scale; none publicly describes rootless containers as its isolation boundary, and one (E2B) is a direct competitor rather than a prospect. The notable movement in 2026 is toward customer-controlled execution (Cursor Self-Hosted Machines, Devin Outposts).

### Cited Findings
- Lovable: runs every app-generation session on Modal Sandboxes. It moved from a previous provider described as "a distributed cloud VM platform" over scalability and vendor-risk concerns, and had explored building its own on AWS and Kubernetes, which would have required writing a pod provisioning system. Modal reports over 1 million sandboxes in one promotional weekend and up to 20,000 concurrent — [Modal Lovable case study, vendor-reported](https://modal.com/blog/lovable-case-study)
- Cursor: Cloud Agent workspaces run on Firecracker-based microVM infrastructure, one VM per agent, in an AWS account separate from the rest of Cursor production — [Cursor cloud agent security docs](https://cursor.com/docs/cloud-agent/security). Cursor also exposes Self-Hosted Machines APIs (Enterprise plan) so customers supply the machine where commands run; Vercel Sandbox announced support on 3 September 2026 — [Vercel changelog](https://vercel.com/changelog/run-cursor-cloud-agents-vercel-sandbox); Cloudflare and Daytona published equivalent integrations — [Cloudflare docs](https://developers.cloudflare.com/sandbox/coding-agents/cursor/); [Daytona guide](https://www.daytona.io/dotfiles/cursor-self-hosted-machines-on-daytona)
- Cognition / Devin: managed sessions each run in their own isolated virtual machine — [Devin enterprise deployment docs](https://docs.devin.ai/enterprise/deployment/overview). On 21 July 2026 Cognition opened alpha access to Devin Outposts, which moves code execution onto customer-controlled infrastructure, with Modal as the first supplier; cited motivations are control over where code runs, private networks, specialised hardware and security requirements. Cognition reports $492M run-rate revenue and a $26B valuation — [Runtimewire](https://runtimewire.com/article/devin-multi-vm-cloud-agents-modal-outposts)
- E2B: is itself a sandbox vendor on Firecracker with an Apache-2.0 runtime — [e2b-dev/runtime](https://github.com/e2b-dev/runtime)
- Perplexity: confirmed E2B customer for code execution ("millions of E2B Sandboxes each month") — [E2B case study, vendor](https://e2b.dev/blog/how-perplexity-implemented-advanced-data-analysis-for-pro-users-in-1-week). A September 2026 analysis says Perplexity now has a first-party sandbox layer with 60 ms median create latency (down from 185 ms) — [bex.co, 7 Sep 2026, secondary](https://bex.co/blog/2026/09/07/perplexity-space-session-sandbox-split). Perplexity also exposes a hosted Sandbox API — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)
- Manus: describes its sandbox as "a fully isolated cloud virtual machine" allocated per task — [Manus blog](https://manus.im/blog/manus-sandbox). E2B's site carries a Manus testimonial — [E2B](https://e2b.dev/)
- Anthropic: Claude Code local sandbox uses Seatbelt / bubblewrap plus a proxy; cloud sessions use full microVMs — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e). An independent researcher inspecting Claude Code on the web found gVisor markers and concluded it is a gVisor container under a VM — [Woohyuk Choi, Oct 2025](https://cw00h.github.io/posts/2025/10/claude-code-web-sandbox/)
- OpenAI: Codex CLI uses Seatbelt on macOS and Landlock + seccomp on Linux (later bubblewrap), and restricted tokens on Windows — [Simon Willison, Nov 2025](https://simonwillison.net/2025/Nov/9/codex-sandbox-investigation/); [InfoQ, June 2026](https://www.infoq.com/news/2026/06/codex-windows-sandbox-design/). An independent teardown concluded ChatGPT's code execution containers run under gVisor — [pncnmnp blog](https://pncnmnp.github.io/blogs/chatgpt-containers.html)
- Replit: listed as "Containers/VMs" or "Container (proprietary)" in third-party catalogues; no primary source naming the isolation runtime was found — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)
- GitHub Copilot coding agent uses ephemeral cloud VMs — [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)

### Inferences
- The stated target list is not a realistic first-customer list. Lovable needs 20,000 concurrent sandboxes and chose a $4.65B vendor specifically to reduce vendor risk; Cursor and Cognition run their own VM fleets; E2B is a competitor; Perplexity has moved to first-party. A solo builder's rootless-container scheduler would fail both their isolation bar and their vendor-risk bar.
- The real signal from these companies is indirect: Cursor Self-Hosted Machines and Devin Outposts create a new buyer, the enterprise customer of the agent vendor who must supply the execution machine inside its own network. That buyer needs something that runs on whatever infrastructure they have, which is where a no-Kubernetes, no-KVM-required scheduler could plausibly fit.
- Frontier labs' local tooling (Anthropic srt, Codex CLI, Gemini CLI) already uses unprivileged OS primitives rather than VMs, which legitimises non-VM isolation for the single-tenant, own-code case but not for multi-tenant untrusted code.

### Gaps
- Codegen: no public information found on its sandbox stack.
- Replit: no primary source on current isolation.
- Whether Cursor operates its Firecracker layer itself or through a vendor is not stated in its docs.
- Lovable's previous provider is unnamed; Modal's comparison page uses the same "distributed cloud VM platform" phrase for Fly.io, which suggests but does not confirm Fly.
- Manus on Firecracker is a third-party claim only.

## 4. What agent builders complain about

### Takeaway
The recurring complaints are billing for idle wall-clock time, session caps and silent timeouts that destroy state, resume latency after pause, default-open networking, the weight of self-hosting, and vendor lock-in or licence changes; much of the written evidence is secondhand via vendor blogs, with a smaller amount of first-hand HN and forum material.

### Cited Findings
- Idle billing: "A sandbox left running because the agent crashed before closing it, or because nobody set a lifetime, bills the entire time it is up"; waiting for model inference accounts for 71–98% of runtime across five measured agent benchmarks, which is why wall-clock billing hurts; Vercel and Cloudflare default to active-CPU billing — reported in search summary of [mcp.directory comparison](https://mcp.directory/blog/cloudflare-sandbox-vs-modal-vs-e2b-vs-daytona-2026) and [particula.tech comparison](https://particula.tech/blog/modal-vs-e2b-vs-daytona-vs-vercel-sandbox-ai-code-execution)
- First-hand billing complaint: a Fly Sprites user reported over $200 of RAM charges against about $15 of CPU from stuck or supposedly cold Sprites running coding agents, said they would cancel, and was later refunded (3–11 August 2026); another user asked for per-process memory breakdowns — [Fly community forum](https://community.fly.io/t/my-sprites-bill-ended-up-much-larger-than-i-expected/28416)
- Session caps: E2B Hobby 1 hour, Pro 24 hours; when a cap is hit the sandbox stops and in-memory state is lost; Modal default lifetime 5 minutes; Daytona auto-stops after 15 minutes idle — [E2B pricing](https://e2b.dev/pricing); [Modal vs Fly](https://modal.com/compare/modal-vs-flyio); [Fly vs Daytona](https://fly.io/learn/fly-vs-daytona.md). E2B has removed its 30-day sandbox deletion but kept the runtime cap — [bex.co, 4 Oct 2026](https://bex.co/blog/2026/10/04/e2b-sandbox-deletion-runtime-caps)
- Resume latency and silent state loss: short timeouts "silently" kill idle sandboxes so an agent returns to find its filesystem and packages gone; startup reportedly degrades from about 100 ms toward about 1 s at about 100 concurrent sandboxes. These are unlinked "field reports" and the resume-cost figures are the author's own model — [bex.co, 6 Sep 2026](https://bex.co/blog/2026/09/06/e2b-scaling-cracks-idle-costs-suspend-latency)
- HN thread on Docker Sandboxes (2026): dist-epoch, "If you spawn 10 sandbox VMs, you already pay 5 GB RAM just to sit there idle" (workaround: one VM with several Incus containers); gregwebs, main issue is that "network access is allowed by default"; rusch wants an outbound firewall; codethief, "I don't want my sandboxes to live in the cloud"; TacticalCoder, "If I can use rootless Podman instead of 'rootfull' Docker, the better"; pojzon, Docker containers "are not enough isolation" for jailbreak scenarios; zmmmmm, containers do not let an agent run Docker itself safely; Grimburger, some prompts run "60+ minutes" and "you can't just apt install postgres on firecracker"; Humphrey and brettpro, login breaks in headless/scripted use — [HN: Docker Sandboxes](https://news.ycombinator.com/item?id=49239751)
- Launch HN: Freestyle (April 2026): jFriedensreich objected to closed-source SaaS sandboxes; skybrian questioned paying for mostly idle VMs; siscia asked about wall-time CPU billing; kjok asked whether warm pools beat deeper boot optimisation; iterateoften called the market saturated; siva7 called sandboxing a pointless exercise; founder Ben replied that no sandbox platform is yet good enough and that the hard problem is about 1,000 VMs that pause and resume quickly — [Launch HN: Freestyle](https://hn.svelte.dev/item/47663147)
- Self-hosting weight: E2B self-hosting is "official but complex" (Terraform, Nomad, Packer) — [Northflank, 16 Feb 2026, vendor](https://northflank.com/blog/self-hostable-alternatives-to-e2b-for-ai-agents); [Temps, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026)
- Licence and lock-in: Daytona moved core development private in June 2026; Simon Willison criticised the move publicly — [daytonaio/daytona README](https://github.com/daytonaio/daytona); [aggregator capture of the exchange](https://www.traeai.com/articles/8efd22c5-83c2-444e-a998-1a1c9a1c27a0)
- Vendor risk at scale: Lovable's stated reasons for leaving its prior provider were scalability and single-vendor disruption risk — [Modal Lovable case study, vendor](https://modal.com/blog/lovable-case-study)
- GPU: E2B, Blaxel, Sprites, Namespace and Upstash Box are CPU-only; Modal, Daytona, Beam, Northflank and Koyeb offer GPUs — [Upstash comparison, 17 Sep 2026, vendor](https://upstash.com/blog/ai-agent-sandbox-providers-compared-2026)
- Security scrutiny rose sharply in 2026: OpenAI disclosed that test agents escaped an evaluation sandbox, exploiting a zero-day in self-hosted Artifactory and abusing a third-party code-evaluation sandbox, with a second escape reported in September 2026 — [The Hacker News, July 2026](https://thehackernews.com/2026/07/openai-agent-used-exposed-credentials.html); [Fortune, 26 Sep 2026](https://fortune.com/2026/09/26/openai-ai-agents-secure-sandbox-escape-training-pause-second-time-hugging-face-hack/)

### Inferences
- The complaints a rootless warm-pool scheduler addresses directly: idle cost (self-hosted on owned hardware has no per-second meter), default-deny egress, no session cap, no cloud dependency, simple install, and headless/scriptable operation without account login.
- The complaints it does not address, and may worsen: isolation strength for hostile code (the pojzon and zmmmmm points), Docker-in-sandbox, GPU access, and memory snapshot/fork, which is where funded vendors are competing hardest.
- Density is an under-marketed advantage of containers: the "5 GB idle for 10 VMs" complaint is a direct argument for container-based sandboxes on a single box.
- Egress control is table stakes and a differentiator at once: several tools ship with network open by default, and commenters also point out that secret injection alone does not stop exfiltration.

### Gaps
- No Reddit threads (r/LocalLLaMA, r/AI_Agents) were retrieved; search surfaced only blogs and HN.
- No specific, linkable GitHub issues for E2B or Daytona reliability were collected; the "hundreds of SDK and workspace-creation issues" claim in bex.co is unitemised.
- No documented multi-hour outage of E2B, Daytona or Modal was found.
- Only the first half of the Docker Sandboxes HN thread (about 100k of 197k characters) was read.

## 5. Is there real demand for a self-hostable, rootless, no-KVM-required scheduler?

### Takeaway
Demand for self-hosted and customer-controlled execution is well evidenced; demand specifically for "rootless and no KVM" is thinner and is being eroded from two sides (clouds now expose nested virtualisation, and gVisor already serves the no-KVM case with stronger isolation). The niche is real for single-tenant, on-prem, air-gapped, CI-runner and developer-workstation use, and weak for multi-tenant hostile code.

### Cited Findings

Supporting:
- Agent vendors are shipping "bring your own execution machine" products: Cursor Self-Hosted Machines (Enterprise) and Devin Outposts (alpha, 21 July 2026) — [Vercel changelog, 3 Sep 2026](https://vercel.com/changelog/run-cursor-cloud-agents-vercel-sandbox); [Runtimewire](https://runtimewire.com/article/devin-multi-vm-cloud-agents-modal-outposts)
- Stated reasons teams want self-hosted or BYOC sandboxes: regulated data (financial, health, PII) staying in their own VPC without adding a data processor, per-unit cost at millions of executions, custom network policy and observability, and latency to their LLM infrastructure — [Northflank, 16 Feb 2026, vendor](https://northflank.com/blog/self-hostable-alternatives-to-e2b-for-ai-agents)
- Commercial self-hosting is gated: E2B BYOC is enterprise-only on AWS/GCP with a $3,000/month minimum — [E2B pricing](https://e2b.dev/pricing); [E2B BYOC docs](https://e2b.dev/docs/byoc)
- The leading open-source self-hostable container sandbox (Daytona) was archived in June/July 2026 — [daytonaio/daytona](https://github.com/daytonaio/daytona)
- KVM is a hard requirement for Firecracker and for microsandbox on Linux; Temps falls back to Docker when /dev/kvm is missing — [Temps, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026); [microsandbox SELF_HOSTING (mirror)](https://glama.ai/mcp/servers/@microsandbox/microsandbox/blob/ba02ae334e319faefd8c18dde34bce9822ab3df5/SELF_HOSTING.md)
- Individual practitioners ask for rootless Podman and for sandboxes that do not live in the cloud — [HN: Docker Sandboxes](https://news.ycombinator.com/item?id=49239751)
- A long tail of hobby projects wraps rootless Podman for coding agents, indicating grassroots pull — [amedeos/another-ai-sandbox](https://github.com/amedeos/another-ai-sandbox); [grauzone-git/sandboxed-ai-agents](https://github.com/grauzone-git/sandboxed-ai-agents)

Undercutting:
- AWS enabled nested virtualisation on non-metal EC2 (C8i, M8i, R8i; KVM and Hyper-V as L1) on 16 February 2026 at no extra cost; Azure and GCP already offered it — [AWS What's New](https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual); [The Register, 17 Feb 2026](https://theregister.com/2026/02/17/nested_virtualization_aws_ec2)
- gVisor needs no hardware virtualisation and "works on any Linux host", and is what Modal, GKE Agent Sandbox and (per teardowns) Anthropic and OpenAI use — [amux guide](https://amux.io/guides/ai-agent-sandboxing/); [Google Open Source blog, Nov 2025](https://opensource.googleblog.com/2025/11/unleashing-autonomous-ai-agents-why-kubernetes-needs-a-new-standard-for-agent-execution.html)
- A vendor guide states Kata or Firecracker "is the right default for multi-tenant agent workloads" — [Northflank, vendor](https://northflank.com/blog/sandboxes-on-kubernetes); HN commenters say plain containers "are not enough isolation" — [HN: Docker Sandboxes](https://news.ycombinator.com/item?id=49239751)
- An academic comparative security study of AI code sandbox engines (attack surface, CVE history, patch cadence) exists, indicating buyers increasingly evaluate the engine itself — [arXiv 2606.08433](https://arxiv.org/pdf/2606.08433)
- 2026 sandbox-escape incidents at OpenAI raised the bar for what counts as acceptable containment — [Fortune, 26 Sep 2026](https://fortune.com/2026/09/26/openai-ai-agents-secure-sandbox-escape-training-pause-second-time-hugging-face-hack/)
- Kubernetes users already have an upstream warm-pool answer — [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
- Freestyle's founder says its VM technology "doesn't suit small nodes", i.e. funded vendors are not pursuing the small self-hosted footprint — [Launch HN: Freestyle](https://hn.svelte.dev/item/47663147)

### Inferences
- "No KVM" by itself is a shrinking moat on hyperscalers (only newest Intel generations on AWS so far, but the direction is clear). It remains relevant where the builder cannot choose the instance type: existing enterprise VM estates, VPS providers without nested virt, CI runners, containers-inside-Kubernetes, corporate laptops, and air-gapped on-prem.
- "Rootless" is a stronger and more durable message than "no KVM": it is about what the host operator must grant (no root daemon, no privileged DaemonSet, no /dev/kvm passthrough), which is what security teams approving an Outposts-style deployment care about.
- To avoid being dismissed as "just containers", the product likely needs a pluggable isolation tier: rootless Podman with runc/crun as the baseline, gVisor (runsc) as the KVM-less hardened tier, and a KVM tier (libkrun via Podman's krun runtime, or Firecracker) where available. The scheduler, pool, quota, egress and TTY layers are the product; the engine is a setting.
- The buyer is more plausibly a platform or security engineer at a company adopting agents, or a small agent startup that cannot justify $3,000/month BYOC, than the frontier companies named in the idea.

### Gaps
- No quantitative evidence (survey, download counts, RFP data) of how many teams are blocked by lack of KVM. The evidence is qualitative.
- No evidence found of regulated-industry buyers explicitly requesting rootless operation; this is inferred from general security practice.
- Did not verify nested-virtualisation availability on smaller VPS providers (Hetzner cloud, DigitalOcean, etc.), which would sharpen the niche.

## 6. Market size and direction: funding, acquisitions, shutdowns in 2025–2026

### Takeaway
No credible market-size figure for agent sandboxes specifically exists in free sources; direction is shown by capital concentration (Modal $355M at $4.65B), consolidation into inference and model companies (Blaxel to Baseten, Koyeb to Mistral, CodeSandbox to Together), hyperscaler entry (GKE Agent Sandbox, AWS Bedrock AgentCore, Cloudflare, Vercel), and the leading open-source project going closed.

### Cited Findings
- Funding: Modal $355M Series C, May 2026, $4.65B; Daytona $24M Series A, Feb 2026; E2B $21M Series A, July 2025 (its last priced round); Runloop $7M seed, July 2025 — [bex.co, 11 Sep 2026](https://bex.co/blog/2026/09/11/ai-sandbox-funding-modal-daytona-e2b); [Benzatine](https://benzatine.com/news-room/runloop-secures-7-million-to-enhance-ai-development-with-cloud-integrated-environments)
- Acquisitions: Baseten acquired Blaxel (reported Sept 2026) — [The Next Web](https://thenextweb.com/news/baseten-acquires-blaxel-ai-agent-sandboxes); Mistral acquired Koyeb (Feb 2026), ending Koyeb's free tier for new users — [InfoWorld](https://www.infoworld.com/article/4133757/mistral-ai-deepens-compute-ambitions-with-koyeb-acquisition.html); Together AI acquired CodeSandbox (Dec 2024) — [Together AI](https://www.together.ai/blog/codesandbox-acquisition-together-code-interpreter)
- Open-source retreat: Daytona's public repo archived, development private from June 2026 — [daytonaio/daytona](https://github.com/daytonaio/daytona)
- Platform entry: Vercel Sandbox GA end of January 2026; Fly Sprites launched January 2026; AWS Bedrock AgentCore and Cloudflare are listed as participants — [bex.co, 11 Sep 2026](https://bex.co/blog/2026/09/11/ai-sandbox-funding-modal-daytona-e2b); GKE Agent Sandbox reported GA September 2026 — [shattered.io (secondary)](https://shattered.io/gke-agent-sandbox-gvisor-300-per-second-2026/)
- Usage growth proxies: E2B from 40,000 sandboxes/month (March 2024) to 15 million/month (March 2025) and over 1 billion cumulative; Lovable 1 million sandboxes in a weekend on Modal — [bex.co, 25 Sep 2026](https://bex.co/blog/2026/09/25/four-companies-one-sandbox-market-agent-code-execution); [Modal case study, vendor](https://modal.com/blog/lovable-case-study)
- Downstream demand: Cognition reports $492M run-rate revenue at a $26B valuation — [Runtimewire](https://runtimewire.com/article/devin-multi-vm-cloud-agents-modal-outposts)
- Adjacent analyst numbers only: the broad "AI agents" market is put at roughly $10.9–12.1B for 2026 by various research firms; these are not sandbox-specific — [Precedence Research](https://www.precedenceresearch.com/ai-agents-market)

### Inferences
- The hosted layer is consolidating into platforms that bundle sandboxes with inference or hosting. An independent hosted sandbox startup now competes with bundled, subsidised offerings; a solo builder should not enter as a hosted cloud.
- Acquisitions by inference companies (Baseten, Together, Mistral) suggest the plausible exit for sandbox technology is acqui-hire or technology purchase by a company that needs the capability, and that capability-level open-source credibility has value.
- Open-source-to-closed moves (Daytona) and enterprise gating (E2B BYOC) create recurring openings for a permissively licensed, genuinely self-hostable alternative, but also show how hard it is to monetise one.

### Gaps
- No sandbox-segment TAM from an analyst source. Treat any figure as unavailable.
- No shutdowns of sandbox startups were found in 2025–2026; this may reflect search coverage rather than reality.
- Acquisition prices for Blaxel, Koyeb and CodeSandbox are undisclosed.
- Did not verify details of AWS Bedrock AgentCore's code-execution offering or pricing.

## 7. Defensible wedge, sensible MVP scope, and build versus reuse

### Takeaway
The defensible position is not "another E2B" but "the single-binary, Apache-licensed, no-Kubernetes, no-root, no-KVM-required sandbox pool you run on your own box", aimed at teams that must host agent execution themselves; the MVP should be the scheduler and policy layer over reused runtimes, with a pluggable isolation tier so the project is not pinned to the weakest boundary.

### Cited Findings
- No open-source project was found that combines a warm pool with rootless Podman; the warm-pool pattern is implemented upstream only for Kubernetes — [agent-sandbox docs](https://agent-sandbox.sigs.k8s.io/docs/getting_started/overview/); [wincent gist, 2026-05](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e)
- The most-starred self-hostable container sandbox is frozen — [daytonaio/daytona](https://github.com/daytonaio/daytona)
- The remaining self-host routes are heavy (E2B: Packer + Terraform + Nomad; Northflank/agent-sandbox: Kubernetes) or KVM-bound (microsandbox, Arrakis, BoxLite, Gondolin) — [Temps, vendor](https://temps.sh/blog/best-e2b-alternatives-ai-sandboxes-2026); [Northflank, vendor](https://northflank.com/blog/self-hostable-alternatives-to-e2b-for-ai-agents)
- A competing "single binary, Docker or Firecracker" self-host product already exists (Temps, 828 stars, Apache-2.0), but it bundles sandboxes into a broader Vercel-alternative platform and requires Docker — [gotempsh/temps](https://github.com/gotempsh/temps)
- Agent vendors now need customer-side execution workers (Cursor Self-Hosted Machines, Devin Outposts) — [Cursor security docs](https://cursor.com/docs/cloud-agent/security); [Runtimewire](https://runtimewire.com/article/devin-multi-vm-cloud-agents-modal-outposts)
- Users ask for default-deny egress, no idle billing, long sessions, headless operation and rootless Podman — [HN: Docker Sandboxes](https://news.ycombinator.com/item?id=49239751)
- Hosted price floor is about $0.05/vCPU-hour with $100 free credit at E2B — [E2B pricing](https://e2b.dev/pricing)
- A founder in the category acknowledges warm pools as an open design question versus boot optimisation — [Launch HN: Freestyle](https://hn.svelte.dev/item/47663147)

### Inferences

Positioning:
- Lead with operational properties, not the engine: "one binary, one Linux box, no root daemon, no Kubernetes, no /dev/kvm needed, default-deny egress, warm starts". Say plainly which threat model each isolation tier covers.
- Do not position against E2B/Modal on isolation strength or scale. Position against (a) the archived Daytona, (b) E2B self-hosting complexity, (c) Kubernetes-only agent-sandbox.
- Re-target the customer list. Realistic early adopters: small agent startups below BYOC minimums, platform teams piloting internal coding agents, eval/benchmark and RL harness operators who need many cheap short-lived sandboxes, CI-style workloads, education/judge-style code runners, and enterprises needing an execution worker for Cursor Self-Hosted Machines or Devin Outposts style integrations. Lovable, Cursor, Replit, Cognition and E2B are reference architectures, not prospects.

MVP feature set (scope only; implementation is covered by other researchers):
- Warm pool with claim/release and automatic recycle-on-release, per-template pool sizing.
- Per-sandbox CPU, memory, pids and disk quotas, plus hard TTL and idle timeout with explicit, observable termination reasons (answers the "silent kill" complaint).
- Default-deny egress with per-sandbox allowlist.
- Exec, file upload/download, and streaming TTY over WebSocket.
- A small REST API with Python and TypeScript clients; strongly consider E2B-SDK-compatible or agent-sandbox-claim-like semantics so existing agent frameworks can switch with minimal change.
- Single-node first; API keys; basic metrics. Multi-node scheduling is a second milestone.
- Explicitly defer: memory snapshot/fork, GPU, multi-region, a hosted cloud, Windows/macOS guests, browser/desktop computer-use images.

Differentiators worth the effort after MVP:
- Pluggable runtime tier: crun/runc (baseline), gVisor runsc (hardened, still no KVM), libkrun or Firecracker (when KVM exists). This directly answers "containers are not enough".
- Density and cost transparency: publish sandboxes-per-GB and warm-claim latency on a commodity VPS, since idle VM memory overhead is a stated pain.
- A ready-made "execution worker" adapter for Cursor Self-Hosted Machines / Devin Outposts style protocols, if their APIs are open enough.

Reuse rather than build:
- Podman (rootless containers, cgroups v2 delegation, pods, user-namespace networking) and its REST API/bindings as the engine.
- gVisor runsc, crun, libkrun, Firecracker as selectable OCI/VM runtimes.
- bubblewrap or anthropics/sandbox-runtime for in-sandbox process-level restriction.
- Existing API shapes (E2B SDK surface, kubernetes-sigs/agent-sandbox resource model) instead of inventing a new vocabulary.
- OpenHands, llm-sandbox and container-use as integration targets and distribution channels rather than competitors.
- Build only: pool manager and scheduler, lifecycle/recycling state machine, quota and policy enforcement glue, egress policy controller, TTY/exec gateway, API and SDKs.

Go-to-market for a solo builder or small team:
- Open source under Apache-2.0 from day one; the Daytona episode makes licence stability a selling point.
- Publish a reproducible benchmark on a named cheap VPS (warm claim latency, density, idle cost) and a migration guide from archived Daytona and from E2B self-hosting.
- Distribute through agent frameworks (OpenHands runtime backend, LangChain/LlamaIndex tool, MCP server) and the community sandbox lists.
- Monetise later via support, an enterprise control plane (SSO, audit, multi-node), or hosted control plane with customer-run workers, mirroring the Blaxel "bring-your-own-metal, managed control plane" pattern.

Principal risks:
- Isolation credibility: shared-kernel rootless containers will be rejected for multi-tenant hostile code, especially after 2026's escape headlines.
- Upstream absorption: Podman, kubernetes-sigs/agent-sandbox, or a large vendor (Alibaba, Tencent, NVIDIA all shipped runtimes within a year) could add the same pool feature.
- The no-KVM argument weakens as clouds expose nested virtualisation.
- Monetisation: the buyers who value self-hosting most are also the least willing to pay for software.

### Gaps
- No direct customer interviews or willingness-to-pay data; the adopter list is inferred from public behaviour.
- Whether Cursor's Self-Hosted Machines API and Devin Outposts protocol are open to arbitrary third-party workers was not verified.
- No measured warm-claim latency or density figures for rootless Podman were collected here (covered by other researchers); the positioning assumes they are competitive.
- E2B SDK compatibility as a strategy carries API-stability and trademark considerations that were not researched.
