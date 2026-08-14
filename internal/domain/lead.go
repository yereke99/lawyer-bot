package domain

import "time"

// LeadStatus is the sales lifecycle position of a lead.
type LeadStatus string

const (
	LeadNew        LeadStatus = "new"
	LeadQualifying LeadStatus = "qualifying"
	LeadQualified  LeadStatus = "qualified"
	LeadContacted  LeadStatus = "contacted"
	LeadConverted  LeadStatus = "converted"
	LeadClosed     LeadStatus = "closed"
	LeadRejected   LeadStatus = "rejected"
)

// Valid reports whether s is a known lead status.
func (s LeadStatus) Valid() bool {
	switch s {
	case LeadNew, LeadQualifying, LeadQualified, LeadContacted,
		LeadConverted, LeadClosed, LeadRejected:
		return true
	}
	return false
}

// Lead sources. The source may be supplied by an external campaign parameter
// carried in the WhatsApp referral payload.
const (
	SourceWhatsApp    = "whatsapp"
	SourceInstagram   = "instagram"
	SourceWebsite     = "website"
	SourceAdvertising = "advertising"
	SourceUnknown     = "unknown"
)

// Lead is a qualified (or in-progress) sales opportunity.
type Lead struct {
	ID                   int64
	UserID               int64
	ServiceCode          string
	ServiceName          string
	Language             Language
	PhoneNumber          string
	LeadScore            float64
	Status               LeadStatus
	Source               string
	QualificationSummary string
	NotifiedAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
