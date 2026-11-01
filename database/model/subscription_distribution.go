package model

// Templates contain administrator-owned client rules, never provider credentials.
type SubscriptionTemplate struct {
	Id       string `json:"id" gorm:"primaryKey;size:64"`
	Name     string `json:"name"`
	Format   string `json:"format"`
	Content  string `json:"-"`
	Enabled  bool   `json:"enabled"`
	Revision uint64 `json:"revision"`
}
type ClientSubscriptionTemplate struct {
	ClientId   uint   `gorm:"primaryKey;index"`
	Format     string `gorm:"primaryKey;size:16"`
	TemplateId string `gorm:"index;size:64"`
}

// Dedicated legacy hosts remain reserved after an alias is deleted. An old URL
// can never fall through into a native subscription route after revocation.
type LegacySubscriptionHost struct {
	Host string `gorm:"primaryKey;size:253"`
}
type LegacySubscriptionAlias struct {
	Id              string `json:"id" gorm:"primaryKey;size:64"`
	Name            string `json:"name"`
	Host            string `json:"host" gorm:"index;size:253"`
	LookupKey       string `json:"-" gorm:"uniqueIndex;size:64"`
	ClientId        uint   `json:"clientId" gorm:"index"`
	Enabled         bool   `json:"enabled"`
	Expiry          int64  `json:"expiry"`
	DefaultFormat   string `json:"defaultFormat"`
	ClashTemplateId string `json:"clashTemplateId"`
	JsonTemplateId  string `json:"jsonTemplateId"`
	Revision        uint64 `json:"revision"`
}
