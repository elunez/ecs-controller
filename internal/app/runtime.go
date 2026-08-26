package app

// RuntimeStatus describes the latest observed state of a background component.
// Error and detail fields must never contain credentials or other secrets.
type RuntimeStatus struct {
	Key            string `json:"key"`
	Label          string `json:"label"`
	Status         string `json:"status"`
	LastStartedAt  int64  `json:"last_started_at"`
	LastSuccessAt  int64  `json:"last_success_at"`
	LastFailureAt  int64  `json:"last_failure_at"`
	LastDurationMS int64  `json:"last_duration_ms"`
	NextRunAt      int64  `json:"next_run_at"`
	LastError      string `json:"last_error"`
	Detail         string `json:"detail"`
	UpdatedAt      int64  `json:"updated_at"`
}
