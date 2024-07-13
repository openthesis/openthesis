# Design

The design docs are meant to brainstorm various ideas related to each component in Openthesis.

*Most ideas here are experimental, and could be fairly wrong in some aspects.*

## Deterministic hypervisor

Building a hypervisor from scratch and making it deterministic is one way of doing it, but I'm mostly inclined towards forking an existing hypervisor and making patches inside it.

Possible candidates are:

- [ ] QEMU/KVM
- [ ] Firecracker
- [ ] more?

The idea is to make most (or all) interfaces mocked/simulated in order to gain control over its determinism. What needs to be patched:

- [ ] Time
- [ ] Storage?
- [ ] Network?
- [ ] more? (devices, etc.)

One possible area to explore is to exploit the `VMCALL` instruction and make it as a communication channel to act between the guest and the host. I don't have much clarity on how this could be done, but it would possibly help in making communications easier. Interrupts can help too, and they can be issued from the VMM's side.

## Testbed

Inside the VM, the plan is to have a container orchestrator set up, eg., Docker. A `docker-compose` inside the VM could help in running the user-provided images.

Since network is mocked, and since user applications are expected to mock external APIs, we can periodically swap out a good network bridge with a faulty one to introduce network faults.

Another way to introduce fault injection through the mocked interfaces is to possibly corrupt the Docker volumes.

## Explorer

The explorer would be either AFL++, or a custom one with more exploration options.

Building a custom one would take some time, but it can possibly support more traditional state space search algorithms. A lot of old CS studies have some interesting state space search algorithms, and they *might* complement the current implementations.

I'd also like to support `BUGGIFY`-style macros as a part of code-driven fault injection. However, the core idea for now is to have at least a coverage-guided fuzzer as that seems to be the most suitable type of fuzzer for this project.

Another long-term goal is to support time-travel debugging through a set of snapshots taken by the VMM at certain timestamps, and then load the state when required.