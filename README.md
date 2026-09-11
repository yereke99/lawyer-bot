# lawyer-bot

WhatsApp AI assistant and Admin CRM for a legal services company.

This is **not** a general purpose chatbot. It is a lead qualification system
with a consultant panel: it reads incoming WhatsApp messages, works out whether
they are about legal services, identifies which service the customer needs, asks
the minimum number of questions, keeps durable state per client, follows up
automatically, and lets a human consultant take the conversation over at any
moment from the CRM.

The Admin CRM lives at `/admin` in the same binary. See **[docs/CRM.md](docs/CRM.md)**
for its architecture, the state machine, the follow-up guarantees and the
security model.

Five rules are enforced by the architecture, not just by the prompt:

1. **The bot never starts a conversation.** Every code path that sends a message
   is reachable only from an inbound Green API polling notification or Meta
   webhook.
2. **The bot does not answer every message.** Greetings, "как дела?", "какая
   погода?" are stored, traced, and ignored.
3. **The model never decides whether to reply.** OpenAI returns a
   classification; deterministic Go code in `response_decision.go` decides
   whether a response is allowed. When `LLM_AGENT_REPLIES=true`, OpenAI then
   writes the customer-facing wording for that allowed response.
4. **The assistant and a consultant never both answer.** `crmGate` suppresses
   automation for a blocked, paused, closed or human-owned conversation before a
   single token is spent, and every outgoing message — automatic or manual —
   leaves through one `Messenger` holding a per-chat lock.
5. **The model never sets business state.** It may suggest a stage or a status;
   `statemachine.go` validates the value and the transition against what the
   database holds. Terminal statuses are human decisions only.

## The assistant never writes first

The number is also used by people. Nobody is contacted, answered or enrolled by
automation until they send an activation trigger to it themselves.

```
inbound event
   |
   +-- our own outgoing / status event ---> ignore
   +-- group or broadcast chat ------------> ignore
   |
   v
private inbound message  (stored and traced either way)
   |
   +-- funnel session open? --- yes ------> process with the model
   |
   no
   |
   +-- matches an activation trigger? -- no ---> ignore, stay silent
                                        |
                                       yes
                                        v
                              open the session, then process
```

The session is a column on the client record (`users.bot_activated_at`), so it
survives restarts and is visible in the CRM. It is opened once: a repeated
trigger never restarts a running conversation. Once open, every later private
message from that customer goes to the model, including short answers with no
keyword in them.

`BOT_ACTIVATION_TRIGGERS` configures the phrases, separated by `|`. Matching
folds away case, punctuation, repeated spaces and line breaks, but the whole
phrase must be present. `BOT_ACTIVATION_STRICT=true` narrows activation to those
phrases alone; left false, a clear legal-service keyword also opens a session,
which is the behaviour the business already relies on.

## Pipeline

```
green polling/webhook -> store -> crm gate -> session gate -> gate -> classify -> state machine -> decide -> agent/template -> reply -> qualify -> follow-up
                         |          |            |             |        |            |              |          |               |         |          |
                      always    blocked?     in funnel?      free    OpenAI      validated       Go rules   OpenAI or      WhatsApp  to Diana   durable job
                                human?                                                                      safe fallback
```

Inbound transport is unchanged: Green API **native polling**, no incoming
webhook. The CRM's own live updates use SSE between the browser and this server,
which is a separate concern from how WhatsApp messages arrive.

Every outgoing message — an automatic answer, a follow-up and a consultant's
manual reply — leaves through `internal/service/outbound.go`. It holds the
per-chat send lock, the delivery audit trail, the CRM activity timestamps and a
bounded retry of the transport. A delivery failure retries the message that was
already generated; the model is never called a second time for it.

The **gate** (`internal/service/gate.go`) is the token budget guard. It runs
after the session gate and before any OpenAI call:

| Message                       | In funnel | Model called | Reply |
|-------------------------------|-----------|--------------|-------|
| `Привет`                      | no        | no           | no    |
| `Ты где?`                     | no        | no           | no    |
| activation trigger            | no        | yes          | yes   |
| `Какие у вас услуги?`         | no        | yes*         | yes*  |
| `Иә, рахмет`                  | yes       | yes          | yes   |
| `Какая погода?`               | yes       | yes          | yes   |
| image with no caption         | either    | no           | no    |

\* unless `BOT_ACTIVATION_STRICT=true`.

Outside the funnel nothing costs tokens and nothing is answered. Inside it the
customer is holding a conversation, so their messages are not filtered by
keyword heuristics — dropping them is what leaves a customer talking to silence.

`AI_ANALYZE_UNMATCHED` applies only inside an open session.

## Pricing

No service ships with a fixed price. In agent mode, model-authored customer
replies are filtered before sending; anything containing a price-like token,
currency or suspicious amount is discarded and the bot falls back to the safe
template wording (`SanitizeReply`, `SanitizeQuestion`). To publish a real
price, set `HasFixedPrice` and the `FixedPrice*` fields on that service in
`internal/service/catalog.go`.

## Layout

```
cmd/main.go                          wiring and graceful shutdown
config/config.go                     all env configuration + .env loader
internal/
  domain/                            types only, no dependencies
  handler/     greenapi_polling.go    Green API native polling
               whatsapp.go router.go  Meta webhook termination, no business logic
  service/     qualification.go      the pipeline orchestrator
               gate.go               token budget guard (pre-filter)
               triggers.go           deterministic phrase matching (ru/kk/en)
               response_decision.go  the only place that decides to reply
               catalog.go            legal service catalog
               reply.go              fallback templates, price protection
               lead.go               lead scoring, summary, Diana notification
               phone.go              phone normalisation
  repository/                        SQLite: schema, migrations, queries
  integration/openai/                classification + agent reply client
  integration/whatsapp/              Green API and Cloud API clients + parsers
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
cp .env.example .env      # fill in OPENAI_API_KEY, GREEN_API_*, DIANA_*
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

Green API native polling receives and deletes queue notifications without
waiting for the artificial reply delay; only the outgoing automatic bot reply is
delayed. Lead notifications to Diana are sent without this pacing.

For Green API polling, set:

```env
WHATSAPP_PROVIDER=greenapi
GREEN_API_ID_INSTANCE=...
GREEN_API_TOKEN_INSTANCE=...
GREEN_API_API_URL=https://api.green-api.com
GREEN_API_POLLING_ENABLED=true
LLM_AGENT_REPLIES=true
BOT_ACTIVATION_TRIGGERS=Сәлеметсіз бе! Тауар белгісін тіркегім келеді
```

Green API delivers this account's own sends back as `outgoingMessageReceived`
and `outgoingAPIMessageReceived`. Only `incomingMessageReceived` is treated as
customer input, and an incoming event whose sender is this instance's own WhatsApp
ID is dropped as well, so a consultant's message can never be read as a customer
message and answered.

If the Green API instance has a custom webhook URL configured, clear it in the
Green API cabinet before using `receiveNotification`, otherwise Green API
returns an error for polling.

For Meta mode, set `WHATSAPP_PROVIDER=meta`, point the Meta webhook at
`https://<host>/webhook/whatsapp`, and use `WHATSAPP_VERIFY_TOKEN` for the
subscription challenge. Set `WHATSAPP_APP_SECRET` so signatures are verified.

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

`internal/service/funnel_test.go` covers the routing rules the business depends
on: an unrelated private message before the trigger is silent and costs nothing,
the trigger opens a session and the first answer really reaches the customer's
own chat, a later message with no keyword continues the dialogue with history, a
group never activates anything, a duplicate delivery answers once, two customers
stay isolated, a failed send preserves the generated reply without re-running the
model, a consultant's reply stops the assistant, and the outbound layer refuses
groups and broadcast lists.

The CRM adds coverage for: migration idempotency on a database that already
holds production rows, client lookup and de-duplication, human takeover under
concurrency, AI suppression during takeover, resume, blocking, closing, durable
follow-up scheduling and exclusive claiming, cancellation on reply, stale-claim
recovery, business hours, concurrent messages from one client, the OpenAI
outage path, the WhatsApp failure path, media validation and path traversal,
authentication, CSRF, rate limiting, authorization by role, export contents, and
the state machine's refusal of illegal model suggestions.

## Reliability

- OpenAI down: the error is logged and stored, deterministic triggers become the
  fallback (a clear legal keyword still gets the service menu, anything else
  stays silent rather than guessing).
- WhatsApp down: the failure is recorded in `deliveries`, and lead qualification
  still completes.
- SQLite errors: returned as controlled errors, never a panic in a handler.
- A panic inside one message is recovered by the worker and never stops the bot.
- Retried webhooks and Green API notifications are deduplicated by provider
  message ID, so a customer is never answered twice.
- Per-chat processing is sequenced, so a delayed reply in one chat does not
  reorder that customer's bot messages or block unrelated chats.
- Follow-ups are database rows, not timers: a restart never loses one, a job is
  claimed by exactly one worker, and a claim abandoned by a crashed worker is
  recovered after `FOLLOWUP_CLAIM_TTL_MINUTES`.
- A follow-up is re-validated against the live client immediately before sending,
  so a nudge is never delivered to someone who has already replied, been blocked,
  been taken over or been closed.
