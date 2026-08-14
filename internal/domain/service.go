package domain

// Service codes. Keep in sync with the catalog in internal/service/catalog.go.
const (
	ServiceTrademarkRegistration  = "trademark_registration"
	ServiceBusinessRegistration   = "business_registration"
	ServiceLegalConsultation      = "legal_consultation"
	ServiceContractDrafting       = "contract_drafting"
	ServiceContractReview         = "contract_review"
	ServicePublicOffer            = "public_offer"
	ServicePrivacyPolicy          = "privacy_policy"
	ServiceWebsiteUserAgreement   = "website_user_agreement"
	ServiceMobileAppUserAgreement = "mobile_app_user_agreement"
	ServiceEcommerceDocuments     = "ecommerce_documents"
	ServiceOnlinePlatformDocs     = "online_platform_documents"
	ServicePaymentRefundPolicy    = "payment_refund_policy"
	ServiceDeliveryPolicy         = "delivery_policy"
	ServiceOtherLegalService      = "other_legal_service"
)

// LegalService is one offering in the catalog.
type LegalService struct {
	Code          string
	NameRU        string
	NameKZ        string
	NameEN        string
	DescriptionRU string
	DescriptionKZ string
	DescriptionEN string

	// FixedPrice is shown verbatim only when HasFixedPrice is true. When it is
	// false the bot must fall back to the "cost depends on complexity" wording.
	// The model is never allowed to produce a price of its own.
	HasFixedPrice bool
	FixedPriceRU  string
	FixedPriceKZ  string
	FixedPriceEN  string

	// Clarify holds the single short follow-up question the bot may ask once a
	// service has been identified, per language.
	ClarifyRU string
	ClarifyKZ string
	ClarifyEN string
}

// Name returns the service name in the requested language.
func (s LegalService) Name(lang Language) string {
	switch lang.OrDefault() {
	case LangKK:
		return s.NameKZ
	case LangEN:
		return s.NameEN
	default:
		return s.NameRU
	}
}

// Description returns the service description in the requested language.
func (s LegalService) Description(lang Language) string {
	switch lang.OrDefault() {
	case LangKK:
		return s.DescriptionKZ
	case LangEN:
		return s.DescriptionEN
	default:
		return s.DescriptionRU
	}
}

// ClarifyQuestion returns the configured follow-up question, or "" when the
// service needs no clarification.
func (s LegalService) ClarifyQuestion(lang Language) string {
	switch lang.OrDefault() {
	case LangKK:
		return s.ClarifyKZ
	case LangEN:
		return s.ClarifyEN
	default:
		return s.ClarifyRU
	}
}

// Price returns the configured fixed price, or "" when pricing depends on
// complexity.
func (s LegalService) Price(lang Language) string {
	if !s.HasFixedPrice {
		return ""
	}
	switch lang.OrDefault() {
	case LangKK:
		return s.FixedPriceKZ
	case LangEN:
		return s.FixedPriceEN
	default:
		return s.FixedPriceRU
	}
}
