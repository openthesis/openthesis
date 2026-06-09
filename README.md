# OpenThesis

Open-source deterministic hypervisor platform for automated software testing - inspired by [Antithesis](https://antithesis.com/).

> **Status: under active development.** Certain execution paths may not work as intended. The TCG backend is fully deterministic but slow (~100 states/min). The Firecracker backend is fast (~1200 states/min) and fully deterministic when host KVM patches are applied (`make install-host-kernel`); without them (`no_host_patches: true`, the default for examples), it reaches approximately 99% KCOV edge determinism.

## How it works

OpenThesis runs your system inside a deterministic virtual machine where time, randomness, and network events are fully controlled. At each burst boundary, the orchestrator snapshots the VM state and forks execution with a different fault schedule - building a tree of explored states across thousands of distinct scenarios per minute. Coverage is tracked with kernel KCOV[^1]; new edges drive the search forward via an MCTS-based snapshot selector[^2] using AFL-style edge bucketing[^3], while a UCB1 bandit inspired by MOPT[^4] selects which fault kinds to apply. Test drivers can steer the search using IJON-style `maximize` and `explore` hints[^5]. Subtrees with plateaued coverage discovery are detected using the Chao1 species-richness estimator[^6] and deprioritized. When a property violation is found, the exact seed and fault schedule are saved so the bug can be replayed, shrunk to a minimal reproducer, and causally attributed to the specific fault that triggered it.

[^1]: KCOV: https://docs.kernel.org/dev-tools/kcov.html
[^2]: Alphuzz, MCTS-based greybox fuzzing (ACSAC 2022): https://dl.acm.org/doi/epdf/10.1145/3564625.3564660
[^3]: AFL edge bucketing: https://lcamtuf.coredump.cx/afl/technical_details.txt
[^4]: MOPT, mutator scheduling via PSO (USENIX Security 2019): https://www.usenix.org/conference/usenixsecurity19/presentation/lyu
[^5]: IJON, semantic fuzzing guidance (IEEE S&P 2020): https://github.com/RUB-SysSec/ijon
[^6]: Chao1 species-richness estimator: https://github.com/scikit-bio/scikit-bio/blob/main/skbio/diversity/alpha/_chao1.py#L63C1-L69C47

## Quick start

```bash
# Requires Go 1.25+, Linux, KVM (/dev/kvm)
make install   # download guest kernel, rootfs, and patched Firecracker

# Run a campaign
openthesis campaign --config examples/etcd/openthesis.json --duration 10m --parallel 8

# Browse violations
openthesis find

# Replay a specific violation
openthesis replay --artifact ~/.openthesis/etcd/violations/violation-000-*/

# Shrink to a minimal fault schedule
openthesis shrink --artifact ~/.openthesis/etcd/violations/violation-000-*/ --config examples/etcd/openthesis.json
```

## Requirements

- Linux with KVM (`/dev/kvm`)
- Go 1.25+

### Local setup

```bash
make install                        # download guest kernel, rootfs, patched Firecracker
make install BACKEND=firecracker    # same, prefer Firecracker explicitly
make install BUILD_KERNEL=1         # build guest kernel from source instead of downloading
make install-check                  # verify prerequisites (Docker, QEMU, /dev/kvm)
make install-host-kernel            # patch host KVM modules for full determinism (sudo, ~10 min)
```

### Remote server setup

First-time provisioning and full build on a bare-metal server:

```bash
make setup HOST=ubuntu@YOUR_SERVER          # provision + build all + deploy (~60 min)
make status HOST=ubuntu@YOUR_SERVER         # show which steps are complete
```

Individual steps:

```bash
make provision          HOST=ubuntu@YOUR_SERVER   # base packages, firewall
make build-firecracker  HOST=ubuntu@YOUR_SERVER   # patched Firecracker (~30-60 min)
make build-kernel       HOST=ubuntu@YOUR_SERVER   # deterministic guest kernel (~20 min)
make build-rootfs       HOST=ubuntu@YOUR_SERVER   # Debian bookworm rootfs (~10 min)
make build-host-kernel  HOST=ubuntu@YOUR_SERVER   # patched KVM modules for full determinism (~10 min)
make deploy             HOST=ubuntu@YOUR_SERVER   # deploy openthesis binaries
make deploy-example     HOST=ubuntu@YOUR_SERVER EXAMPLE=etcd
```

## Writing tests

Tests use the OpenThesis SDK to declare properties about your system:

```go
import ot "github.com/openthesis/openthesis/sdk/go/assert"

// Must be true on every execution
ot.Always(balance >= 0, "balance is never negative", map[string]any{"balance": balance})

// Must be true at least once during exploration
ot.Sometimes(isLeader, "a leader is eventually elected", nil)

// Must be reachable
ot.Reachable("failover path exercised", nil)
```

Guidance signals steer the exploration engine toward interesting states:

```go
import "github.com/openthesis/openthesis/sdk/go/guidance"

guidance.Maximize("writes_committed", int64(commitCount))
guidance.Explore("raft_term", int64(term))
```

Available SDKs: **Go**, **Python**, **Java**, **C**, **Rust**.

### Test composer

Scripts in the test bundle are dispatched by the guest agent based on filename prefix:

| Prefix | Behavior |
|--------|----------|
| `first_*` | Run once before the driving phase starts |
| `parallel_driver_*` | Run concurrently throughout the driving phase |
| `serial_driver_*` | Run serially, one at a time |
| `singleton_driver_*` | Run exactly once during the driving phase |
| `finally_*` | Run once after the driving phase ends |

## Backends

| Backend | Status | Throughput | Notes |
|---------|--------|-----------|-------|
| `firecracker` | Recommended | ~1200 states/min | PMC-based virtual clock, patched Firecracker + optional host KVM patches |
| `tcg` | Stable | ~100 states/min | Stock QEMU with software instruction counting, no patches needed |
| `gvisor` | Experimental | ~200 states/2min | Container-level determinism |

## License

Openthesis is distributed under [AGPL-3.0-only](LICENSE).
