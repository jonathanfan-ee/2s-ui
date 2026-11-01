package model

// ExternalSubSnapshot is keyed by the exact upstream URL's SHA-256. Neither
// payloads nor leases are part of the admin status response. The existing
// clients' Links remain the authority for who may read a source.
type ExternalSubSnapshot struct {
	Key         string `json:"key" gorm:"primaryKey;size:64"`
	Payload     string `json:"-"`
	LastSuccess int64  `json:"lastSuccess"`
	LastAttempt int64  `json:"lastAttempt"`
	NextAttempt int64  `json:"nextAttempt"`
	LastError   string `json:"lastError"`
	Failures    int    `json:"-"`
	Paused      bool   `json:"paused"`
	Requested   bool   `json:"requested"`
	LockUntil   int64  `json:"-"`
	Lease       string `json:"-"`
}

// ExternalSubAttempt persists the shared upstream request budget across panel
// restarts. Redirects and failed requests spend the same budget as successes.
type ExternalSubAttempt struct {
	Id uint  `gorm:"primaryKey;autoIncrement"`
	At int64 `gorm:"index"`
}
