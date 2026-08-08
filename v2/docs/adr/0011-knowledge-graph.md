# ADR-0011: Durable knowledge graph for agent context

Status: Accepted (retroactive)

## Context

Hive agents need project memory that outlives a single chat session, but prompt
context is scarce and unsafe to fill with every past note. The knowledge design
keeps that memory layered by scope and privacy — personal, project, org, and
community — and merges only relevant facts when preparing an agent kick
([knowledge design](../design/knowledge-system.md)). The implementation also
needs to work when no external embedding service is available.

## Decision

Represent reusable lessons as typed facts with confidence, sources, tags,
relationships, usage counts, and layer metadata
([knowledge types](https://github.com/kubestellar/hive/blob/v4/v2/pkg/knowledge/types.go)). Store local vault pages and
remote layer clients behind one `KnowledgeAPI`, and prime agents by formatting a
bounded set of selected facts into the kick prompt rather than letting agents
query the store directly ([knowledge API](https://github.com/kubestellar/hive/blob/v4/v2/pkg/knowledge/api.go)).

Use a deterministic term-frequency embedder as the default semantic signal: text
is tokenized, feature-hashed into a 256-dimensional vector, and L2-normalized,
with embeddings cached only for the search/reindex pass
([TF embedder](https://github.com/kubestellar/hive/blob/v4/v2/pkg/knowledge/embedding.go)). Persist fact relationships in
a bbolt-backed graph store with SPO/POS/OSP indexes so facts can be traversed by
subject, predicate, or object ([graph store](https://github.com/kubestellar/hive/blob/v4/v2/pkg/knowledge/graphstore.go)).
The retro lane ingests model-generated lessons only after length, secret, and
deduplication gates, then stores them as ordinary project facts derived from the
source bead or PR ([retro lessons](https://github.com/kubestellar/hive/blob/v4/v2/pkg/knowledge/retro_lesson.go)).

## Consequences

Agents can receive durable, source-backed context without depending on an LLM's
conversation memory or a hosted embedding API. Layer precedence and explicit
promotion preserve the privacy boundary described by the design while still
letting community and org knowledge flow down. The trade-off is a deliberately
simple retrieval model: hashed term-frequency vectors and tag/graph edges are
portable and inspectable, but lower quality than model embeddings, and stale or
incorrect facts still need curation rather than blind injection.
