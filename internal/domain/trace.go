package domain

import "time"

// TraceEvent is one step of the processing pipeline for a single incoming
// message. Every message produces a chain of events sharing one TraceID, which
// makes it possible to answer "why did the bot answer / stay silent?" months
// later, straight from the database.
type TraceEvent struct {
	ID         int64
	TraceID    string
	UserID     int64
	MessageID  int64
	Stage      string
	Decision   string
	Reason     string
	Detail     string // small JSON blob with stage specific values
	DurationMS int64
	CreatedAt  time.Time
}

// Pipeline stages, in the order they normally occur.
const (
	StageWebhookReceived = "webhook_received"
	StageMessageParsed   = "message_parsed"
	StageUserUpserted    = "user_upserted"
	StageMessageStored   = "message_stored"
	StageDuplicate       = "duplicate_skipped"
	StageGate            = "gate_evaluated"
	StageTrigger         = "trigger_matched"
	StageAIRequested     = "ai_requested"
	StageAICompleted     = "ai_completed"
	StageAIFailed        = "ai_failed"
	StageAIRejected      = "ai_response_rejected"
	StageDecision        = "response_decision"
	StageReplyBuilt      = "reply_built"
	StageReplyDelayed    = "reply_delayed"
	StageReplySent       = "reply_sent"
	StageReplyFailed     = "reply_failed"
	StageStateChanged    = "state_changed"
	StageLeadCreated     = "lead_created"
	StageLeadUpdated     = "lead_updated"
	StageNotifySent      = "notification_sent"
	StageNotifyFailed    = "notification_failed"
	StagePipelineDone    = "pipeline_done"
	StagePipelineError   = "pipeline_error"
)

// Trace decisions.
const (
	DecisionRespond = "respond"
	DecisionSilent  = "silent"
	DecisionSkipAI  = "skip_ai"
	DecisionCallAI  = "call_ai"
	DecisionOK      = "ok"
	DecisionError   = "error"
)
