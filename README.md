# lawyer-bot

WhatsApp AI lead qualification bot for a legal services company.

This is **not** a general purpose chatbot. It is a lead qualification system:
it reads incoming WhatsApp messages, works out whether they are about legal
services, identifies which service the customer needs, asks the minimum number
of questions, and hands a qualified lead to Diana.

Three rules are enforced by the architecture, not just by the prompt:

1. **The bot never starts a conversation.** Every code path that sends a message
   is reachable only from an inbound webhook.
2. **The bot does not answer every message.** Greetings, "как дела?", "какая
   погода?" are stored, traced, and ignored.
3. **The model never decides to reply.** OpenAI returns a classification;
   deterministic Go code in `response_decision.go` decides what happens.

## Pipeline

```
webhook -> store -> gate -> classify -> decide -> reply delay -> reply -> qualify -> notify
             |        |         |          |          |          |         |
          always   free     OpenAI     Go rules    timer    template   to Diana
```

The **gate** (`internal/service/gate.go`) is the token budget guard. It runs
before any OpenAI call:

| Message                       | State      | Model called | Reply |
|-------------------------------|------------|--------------|-------|
| `Здравствуйте`                | new        | no           | no    |
| `Как дела?`                   | any        | no           | no    |
| `Какая погода?`               | qualifying | no           | no    |
| `Какие у вас услуги?`         | new        | yes          | yes   |
| `Здравствуйте, нужен юрист`   | new        | yes          | yes   |
| `Уже работает`                | qualifying | yes          | yes   |
| image with no caption         | any        | no           | no    |

Small talk costs zero tokens. A trigger match always earns analysis, even next
to a greeting. Inside an active qualification flow, short answers are analysed
because they are answers to the bot's own question.

Set `AI_ANALYZE_UNMATCHED=false` for the strictest possible token saving: then
only messages that match a legal trigger or continue an active flow reach the
model.

## Pricing

No service ships with a fixed price. Replies are built from templates, so the
model structurally cannot invent an amount. Any model-authored text containing
digits or a currency token is discarded before it reaches the customer
(`SanitizeQuestion`, `containsPrice`). To publish a real price, set
`HasFixedPrice` and the `FixedPrice*` fields on that service in
`internal/service/catalog.go`.

## Layout

```
cmd/main.go                          wiring and graceful shutdown
config/config.go                     all env configuration + .env loader
internal/
  domain/                            types only, no dependencies
  handler/     whatsapp.go router.go webhook termination, no business logic
  service/     qualification.go      the pipeline orchestrator
               gate.go               token budget guard (pre-filter)
               triggers.go           deterministic phrase matching (ru/kk/en)
               response_decision.go  the only place that decides to reply
               catalog.go            legal service catalog
               reply.go              reply templates, price protection
               lead.go               lead scoring, summary, Diana notification
               phone.go              phone normalisation
  repository/                        SQLite: schema, migrations, queries
  integration/openai/                strict JSON-schema classification client
  integration/whatsapp/              Cloud API client + webhook parser
  worker/                            bounded worker pool
traits/logger/                       zap, with phone masking
```

## Storage and tracing

SQLite (`modernc.org/sqlite`, pure Go, no CGO). The schema is created on start
up and is idempotent. Eleven tables, all indexed:

| Table               | What it answers                                       |
|---------------------|-------------------------------------------------------|
| `users`             | who the contact is, state, service, lead score        |
| `messages`          | every message in both directions                      |
| `media_assets`      | metadata of received media (binary is not downloaded) |
| `ai_interactions`   | every model call: tokens, latency, raw response       |
| `leads`             | one open lead per user, status, summary               |
| `trace_events`      | every pipeline step, with the reason for each         |
| `state_transitions` | the full qualification path                           |
| `user_facts`        | extracted facts (platform, app status, …)             |
| `deliveries`        | outbound send outcomes, including failures            |
| `webhook_events`    | raw provider payloads                                 |
| `notifications`     | alerts sent to Diana                                  |

Everything produced while handling one incoming message shares a `trace_id`, so
"why did the bot stay silent?" is a single query:

```sql
SELECT stage, decision, reason, detail, duration_ms
FROM trace_events
WHERE trace_id = '...'
ORDER BY id;
```

Common operational queries:

```sql
-- Token spend today
SELECT COUNT(*), SUM(input_tokens), SUM(output_tokens)
FROM ai_interactions WHERE created_at >= date('now');

-- How often the gate saved a model call, by reason
SELECT reason, COUNT(*) FROM trace_events
WHERE stage = 'gate_evaluated' AND decision = 'skip_ai'
GROUP BY reason ORDER BY 2 DESC;

-- Qualified leads waiting for contact
SELECT id, phone_number, service_code, lead_score, qualification_summary
FROM leads WHERE status = 'qualified' ORDER BY created_at DESC;

-- Sends that failed (no lead is ever lost silently)
SELECT * FROM deliveries WHERE status = 'failed' ORDER BY created_at DESC;
```

## Running

```bash
cp .env.example .env      # fill in OPENAI_API_KEY, WHATSAPP_*, DIANA_*
go run ./cmd              # http server on :8080
```

Set `DRY_RUN=true` to run the entire pipeline — storage, classification,
decisions, tracing — without sending a single WhatsApp message.

Automatic customer replies wait for a randomized, human-like delay before the
WhatsApp send. Configure it with:

```env
WHATSAPP_BOT_REPLY_DELAY_MIN_MS=1500
WHATSAPP_BOT_REPLY_DELAY_MAX_MS=3000
```

The webhook is still acknowledged immediately; only the outgoing automatic bot
reply is delayed. Lead notifications to Diana are sent without this pacing.

Point the Meta webhook at `https://<host>/webhook/whatsapp` and use
`WHATSAPP_VERIFY_TOKEN` for the subscription challenge. Set
`WHATSAPP_APP_SECRET` so signatures are verified; without it the bot logs a
warning at start-up and accepts unsigned payloads.

`GET /healthz` reports status, version and queue depth.

## systemd

On a Linux server, deploy the bot as `lower.service`:

```bash
make run
make status
journalctl -u lower.service -f
```

`make build` compiles `./cmd` into `./bin/lower` and generates a local
`lower.service` from the current project directory. The service sets
`WorkingDirectory` to this directory and passes `ENV_FILE=<project>/.env`, so
the existing config loader keeps using the same `.env` file behavior.

The Makefile refuses to overwrite `/etc/systemd/system/lower.service` unless it
contains this project's safety marker.

## Testing

```bash
go test ./...
go test -race ./...
```

Tests never call OpenAI and never send WhatsApp messages: both clients are
interfaces with stub implementations. The suite covers trigger matching, the
token gate, the response decision engine, lead qualification, reply generation
and price protection, phone normalisation, repositories, webhook parsing,
signature verification and the end-to-end pipeline including the Diana handoff.

## Reliability

- OpenAI down: the error is logged and stored, deterministic triggers become the
  fallback (a clear legal keyword still gets the service menu, anything else
  stays silent rather than guessing).
- WhatsApp down: the failure is recorded in `deliveries`, and lead qualification
  still completes.
- SQLite errors: returned as controlled errors, never a panic in a handler.
- A panic inside one message is recovered by the worker and never stops the bot.
- Retried webhooks are deduplicated by `whatsapp_message_id`, so a customer is
  never answered twice.
- Per-chat processing is sequenced, so a delayed reply in one chat does not
  reorder that customer's bot messages or block unrelated chats.
