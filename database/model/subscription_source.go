package model

// ExternalSubSource owns the upstream URL independently of client credentials.
// URLs are write-only in the admin API and are never copied into client rows.
type ExternalSubSource struct {
	Id        string `json:"id" gorm:"primaryKey;size:64"`
	Name      string `json:"name"`
	URL       string `json:"-"`
	Enabled   bool   `json:"enabled"`
	Revision  uint64 `json:"revision"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ExternalSubGrant is panel-owned, separate from the node-synchronized client
// data. A source id by itself never authorizes a subscription read.
type ExternalSubGrant struct {
	SourceId string `json:"sourceId" gorm:"primaryKey;size:64"`
	ClientId uint   `json:"clientId" gorm:"primaryKey;index"`
}
