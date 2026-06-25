# RunLore — Architecture

The detailed architecture as the **React → Investigate → Learn** flow, with the read-only
data-source fan-out and the learning-loop feedback. (The README's "How it works" diagram is the
one-glance summary; this is the source-of-truth detail.)

```mermaid
flowchart TB
  subgraph REACT["React — triggers and intake"]
    A1["Alertmanager webhook"]
    A2["GitOps failure watch<br/>Flux / Argo informer"]
    A3["Trigger policy<br/>prod · critical · namespace"]
    A4["Coalesce · dedupe · rate-limit"]
    A1 --> A3
    A2 --> A3
    A3 --> A4
  end

  subgraph INV["Investigate — ReAct loop, read-only"]
    B0["Queue — single serialized worker<br/>HA leader election"]
    B1["LoopInvestigator<br/>ReAct: reason → tool → observe"]
    B2["Model provider<br/>Anthropic / Gemini / OpenAI-compatible"]
    B3["Tools<br/>what_changed · metrics · logs · network · cloud · cluster · kb_search"]
    B4["what-changed spine<br/>GitOps → go-git rendered diff"]
    B5["Adversarial verify<br/>can only lower confidence"]
    B6["Redaction<br/>ingress + egress"]
    B7["Action gate<br/>off / suggest / approve / auto<br/>server-authoritative · audited"]
    B0 --> B1
    B1 <--> B2
    B1 --> B3
    B3 --> B4
    B3 --> B6
    B1 --> B5
    B1 --> B7
  end

  subgraph LEARN["Learn — open, portable catalog"]
    C1["Curator<br/>drafts KB entry → GitHub PR"]
    C2["KB repo<br/>Git · OKF-compatible markdown<br/>human-reviewed and merged"]
    C3["Catalog<br/>bleve BM25 index"]
    C4["Instant recall<br/>catalog hit"]
    C5["Outcome ledger<br/>+ Bayesian decay"]
    C1 --> C2 --> C3 --> C4
  end

  subgraph DATA["Data sources — read-only"]
    direction LR
    D1["Kubernetes<br/>Flux / Argo CD"]
    D2["Metrics<br/>VictoriaMetrics / Prometheus"]
    D3["Logs<br/>VictoriaLogs"]
    D4["Network<br/>Hubble / AWS VPC"]
    D5["Cloud<br/>AWS CloudTrail"]
  end

  OUT["Notifier — Slack / Matrix<br/>root cause + confidence + evidence"]

  A4 -->|enqueue| B0
  B3 -->|read-only queries| DATA
  B4 -->|rendered-manifest diff| D1
  B1 -->|findings| OUT
  B1 -->|curate| C1
  C4 -.->|instant recall| B1
  B1 -.->|outcome| C5
  C5 -.->|decay weights recall| C3

  classDef react fill:#dae8fc,stroke:#6c8ebf,color:#112;
  classDef inv fill:#ffe6cc,stroke:#d79b00,color:#311;
  classDef learn fill:#d5e8d4,stroke:#82b366,color:#131;
  classDef data fill:#f5f5f5,stroke:#999999,color:#333;
  classDef out fill:#e1d5e7,stroke:#9673a6,color:#212;
  class A1,A2,A3,A4 react;
  class B0,B1,B2,B3,B4,B5,B6,B7 inv;
  class C1,C2,C3,C4,C5 learn;
  class D1,D2,D3,D4,D5 data;
  class OUT out;
```

## Reading it

- **React** — an Alertmanager alert or a GitOps-failure event passes the trigger policy, is
  coalesced / deduped / rate-limited, and enqueued.
- **Investigate** — a **single serialized worker** (HA leader-elected) runs the ReAct loop: the model
  drives **read-only** tools; the **what-changed spine** diffs the exact GitOps revisions Flux/Argo
  reconciled; findings are adversarially verified, secrets are redacted in *and* out, and any proposed
  action passes the **server-authoritative** gate (with a hash-chained audit log).
- **Learn** — the curator opens a KB pull request; once a human merges it, the catalog re-indexes and
  feeds **instant recall** back into the loop, while the outcome ledger's decay weights future recall.
