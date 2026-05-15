# Devshard Known Issues

This document tracks known open issues and design questions for devshard. These
items are not yet treated as fully root-caused bugs; they are the current set of
problems to investigate, reproduce, and either fix in host/proxy code or settle as
explicit product/protocol behavior.

## 1. Host returns no stream after receipt

**Status**: Unknown root cause.

**Observed behavior**: A host accepts a request and returns a receipt, but then
does not return content chunks and does not emit `MsgFinishInference`.

**Impact**:

- Client-visible response can be empty or delayed until the proxy gives up.
- The inference may need to be treated as a miss even though the host produced a
  receipt.
- The proxy needs a clear policy for whether this is an empty stream, stalled
  response, missed inference, or a separate failure class.

**Open questions**:

- Is the host stuck before model generation, during generation, or while writing
  the streamed response?
- Does the host still hold protocol state that prevents a clean retry/finalize?
- Can the proxy distinguish "receipt but no chunks" from a slow first-token case
  without false positives?

## 2. Host stalls after producing chunks

**Status**: Unknown root cause.

**Observed behavior**: A host starts streaming content chunks and then stops
making progress. In some cases the stream resumes after about a minute; in other
cases it never resumes.

**Impact**:

- The client may see a partial answer followed by a long stall.
- Winner selection is ambiguous after a host has already emitted useful content.
- Retrying or switching winners may be hard because the client has already seen
  partial output from the stalled host.

**Open questions**:

- Is the stall caused by model runtime pauses, network backpressure, HTTP/SSE
  buffering, host event-loop blocking, or proxy-side stream handling?
- Should a host that resumes after a minute be penalized the same way as a host
  that never resumes?
- What is the correct inter-chunk timeout for different model sizes and context
  lengths?

## 3. Some nodes have lower max context than expected

**Status**: Needs measurement and host inventory.

**Observed behavior**: At least one node appears to have a lower maximum context
window than expected for the model/configuration it is advertising.

**Impact**:

- Long prompts can fail or truncate on only a subset of hosts.
- Redundancy can become misleading if some hosts are eligible by escrow but not
  actually capable of serving the request.
- Request routing may need to account for per-host capacity, not just model and
  escrow membership.

**Open questions**:

- Which nodes are affected, and what context length do they actually support?
- Is the mismatch caused by model version, runtime flags, GPU memory, tokenizer
  differences, or host configuration drift?
- Should hosts advertise max context explicitly so the proxy can filter requests
  before dispatch?

## 4. System-wide request limiting across multiple proxies and escrows

**Status**: Open design question.

**Problem**: If multiple proxy instances run with their own escrows, each proxy
can independently send requests to the network. Local rate limiting per proxy or
per escrow does not necessarily cap total system load.

**Impact**:

- Aggregate request volume can exceed what the host network can safely serve.
- Individual proxies may look healthy while the combined load overloads hosts.
- Escrow-local limits do not answer how many concurrent requests the whole
  devshard system should allow.

**Open questions**:

- Should system limits be enforced centrally, per host, per participant, per
  account, per escrow, or some combination?
- Do proxies need a shared coordination layer for admission control?
- Should hosts advertise dynamic capacity and reject or defer excess work?
- How do we allocate capacity fairly across independent proxy operators?

## 5. Counting missed inferences with probabilistic requests

**Status**: Open accounting/protocol question.

**Problem**: With probabilistic requests, not every host is expected to receive
every inference. Missed-inference accounting must distinguish a host that was not
selected from a host that was selected and failed to complete its assigned work.

**Impact**:

- Naive miss counting can unfairly penalize hosts that were never sampled.
- Host performance metrics can become hard to compare if request probability
  differs by host, escrow, or request class.
- Settlement and operator reporting need a consistent definition of "miss".

**Open questions**:

- What is the denominator for a miss rate: all user requests, sampled requests,
  receipt-producing requests, or winner-eligible requests?
- How should receipt-without-content and partial-stream stalls be counted?
- Should ghost probes, speculative secondaries, and regular probabilistic
  requests count differently?
- What evidence is required to mark a probabilistic request as assigned, missed,
  completed, or not applicable?
