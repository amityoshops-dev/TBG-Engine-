package observability

import (
	"encoding/json"
	"log"
	"os"
	"time"
)

// Logger emits single-line structured JSON logs matching the field
// extraction configuration in splunk/props.conf.
type Logger struct {
	out *log.Logger
}

func New() *Logger {
	return &Logger{out: log.New(os.Stdout, "", 0)}
}

// LogEntry is the canonical structured log shape shared across every event
// type emitted by the engine (ledger postings, lien decisions, EOD runs,
// signature failures, etc.).
type LogEntry struct {
	Timestamp     string  `json:"timestamp"`
	LogLevel      string  `json:"log_level"`
	CorrelationID string  `json:"correlation_id,omitempty"`
	Channel       string  `json:"channel"`
	EventType     string  `json:"event_type"`
	AccountID     string  `json:"account_id,omitempty"`
	TxnID         string  `json:"txnid,omitempty"`
	Amount        float64 `json:"amount,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	Status        string  `json:"status,omitempty"`
	LatencyMS     float64 `json:"latency_ms,omitempty"`
	ErrorMessage  string  `json:"error_message,omitempty"`
}

func (l *Logger) Emit(e LogEntry) {
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if e.LogLevel == "" {
		e.LogLevel = "INFO"
	}
	if e.Channel == "" {
		e.Channel = "TBG_CMS"
	}
	b, err := json.Marshal(e)
	if err != nil {
		l.out.Printf(`{"log_level":"ERROR","event_type":"LOG_MARSHAL_FAILURE","error_message":%q}`, err.Error())
		return
	}
	l.out.Println(string(b))
}
