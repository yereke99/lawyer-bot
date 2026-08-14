package service

import (
	"sort"
	"strings"

	"lawyer-bot/internal/domain"
)

// Catalog is the legal service catalog. Business content lives here and never
// inside WhatsApp handlers, so adding a service is a one-entry change.
type Catalog struct {
	byCode map[string]domain.LegalService
	order  []string
}

// NewCatalog builds the default catalog.
func NewCatalog() *Catalog {
	c := &Catalog{byCode: make(map[string]domain.LegalService)}
	for _, s := range defaultServices {
		c.Register(s)
	}
	return c
}

// Register adds or replaces a service. Later registrations win, which lets a
// deployment override wording without touching this file.
func (c *Catalog) Register(s domain.LegalService) {
	if _, exists := c.byCode[s.Code]; !exists {
		c.order = append(c.order, s.Code)
	}
	c.byCode[s.Code] = s
}

// Get returns a service by code.
func (c *Catalog) Get(code string) (domain.LegalService, bool) {
	s, ok := c.byCode[strings.TrimSpace(code)]
	return s, ok
}

// Has reports whether a code is a known service.
func (c *Catalog) Has(code string) bool {
	_, ok := c.byCode[strings.TrimSpace(code)]
	return ok
}

// All returns every service in registration order.
func (c *Catalog) All() []domain.LegalService {
	out := make([]domain.LegalService, 0, len(c.order))
	for _, code := range c.order {
		out = append(out, c.byCode[code])
	}
	return out
}

// Codes returns every service code, sorted, for prompt construction and
// validation of model output.
func (c *Catalog) Codes() []string {
	out := make([]string, 0, len(c.byCode))
	for code := range c.byCode {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// Name returns a service name in the given language, falling back to the code
// when the service is unknown.
func (c *Catalog) Name(code string, lang domain.Language) string {
	if s, ok := c.Get(code); ok {
		return s.Name(lang)
	}
	return code
}

// ServiceFromIntent maps a classified intent onto a catalog code. Intents that
// carry no specific service (greeting, unclear, generic inquiry) return "".
func ServiceFromIntent(intent domain.Intent) string {
	switch intent {
	case domain.IntentTrademark:
		return domain.ServiceTrademarkRegistration
	case domain.IntentBusinessReg:
		return domain.ServiceBusinessRegistration
	case domain.IntentContractRequest:
		return domain.ServiceContractDrafting
	case domain.IntentPrivacyPolicy:
		return domain.ServicePrivacyPolicy
	case domain.IntentPublicOffer:
		return domain.ServicePublicOffer
	case domain.IntentMobileAppDocs:
		return domain.ServiceMobileAppUserAgreement
	case domain.IntentWebsiteDocs:
		return domain.ServiceWebsiteUserAgreement
	case domain.IntentEcommerceDocs:
		return domain.ServiceEcommerceDocuments
	case domain.IntentConsultationRequest:
		return domain.ServiceLegalConsultation
	case domain.IntentOtherLegal:
		return domain.ServiceOtherLegalService
	default:
		return ""
	}
}

// defaultServices is the shipped catalog. Pricing is deliberately open for
// every entry: the specialist quotes after reviewing the details.
var defaultServices = []domain.LegalService{
	{
		Code:          domain.ServiceTrademarkRegistration,
		NameRU:        "Регистрация товарного знака",
		NameKZ:        "Тауар белгісін тіркеу",
		NameEN:        "Trademark registration",
		DescriptionRU: "Проверка, подача заявки и сопровождение регистрации товарного знака.",
		DescriptionKZ: "Тауар белгісін тексеру, өтінім беру және тіркеуді сүйемелдеу.",
		DescriptionEN: "Search, filing and support throughout trademark registration.",
		ClarifyRU:     "Подскажите, знак нужно зарегистрировать в Казахстане или в другой стране?",
		ClarifyKZ:     "Белгіні Қазақстанда тіркеу керек пе, әлде басқа елде ме?",
		ClarifyEN:     "Should the trademark be registered in Kazakhstan or another country?",
	},
	{
		Code:          domain.ServiceBusinessRegistration,
		NameRU:        "Регистрация бизнеса",
		NameKZ:        "Бизнесті тіркеу",
		NameEN:        "Business registration",
		DescriptionRU: "Регистрация ИП или ТОО, подготовка учредительных документов.",
		DescriptionKZ: "ЖК немесе ЖШС тіркеу, құрылтай құжаттарын дайындау.",
		DescriptionEN: "Sole proprietorship or LLP registration and founding documents.",
		ClarifyRU:     "Подскажите, вам нужна регистрация ИП или ТОО?",
		ClarifyKZ:     "Сізге ЖК тіркеу керек пе, әлде ЖШС ме?",
		ClarifyEN:     "Do you need to register a sole proprietorship or an LLP?",
	},
	{
		Code:          domain.ServiceLegalConsultation,
		NameRU:        "Юридическая консультация",
		NameKZ:        "Заңгерлік кеңес",
		NameEN:        "Legal consultation",
		DescriptionRU: "Консультация юриста по вашему вопросу.",
		DescriptionKZ: "Сіздің мәселеңіз бойынша заңгердің кеңесі.",
		DescriptionEN: "A lawyer's consultation on your question.",
		ClarifyRU:     "Опишите, пожалуйста, кратко суть вопроса.",
		ClarifyKZ:     "Мәселенің мәнін қысқаша сипаттап беріңізші.",
		ClarifyEN:     "Could you briefly describe the issue?",
	},
	{
		Code:          domain.ServiceContractDrafting,
		NameRU:        "Составление договора",
		NameKZ:        "Шарт жасау",
		NameEN:        "Contract drafting",
		DescriptionRU: "Разработка договора под вашу задачу.",
		DescriptionKZ: "Сіздің міндетіңізге сай шарт әзірлеу.",
		DescriptionEN: "Drafting a contract tailored to your case.",
		ClarifyRU:     "Подскажите, какой договор нужен и с кем он заключается?",
		ClarifyKZ:     "Қандай шарт керек және ол кіммен жасалады?",
		ClarifyEN:     "What kind of contract do you need, and with whom?",
	},
	{
		Code:          domain.ServiceContractReview,
		NameRU:        "Проверка договора",
		NameKZ:        "Шартты тексеру",
		NameEN:        "Contract review",
		DescriptionRU: "Юридический анализ договора и рекомендации по рискам.",
		DescriptionKZ: "Шартты құқықтық талдау және тәуекелдер бойынша ұсыныстар.",
		DescriptionEN: "Legal analysis of a contract with risk recommendations.",
		ClarifyRU:     "Подскажите, договор уже готов и его нужно проверить?",
		ClarifyKZ:     "Шарт дайын ба, оны тексеру керек пе?",
		ClarifyEN:     "Is the contract already drafted and ready for review?",
	},
	{
		Code:          domain.ServicePublicOffer,
		NameRU:        "Публичная оферта",
		NameKZ:        "Жария оферта",
		NameEN:        "Public offer",
		DescriptionRU: "Подготовка публичной оферты для сайта или сервиса.",
		DescriptionKZ: "Сайт немесе сервис үшін жария оферта дайындау.",
		DescriptionEN: "Public offer for a website or service.",
		ClarifyRU:     "Оферта нужна для сайта или для мобильного приложения?",
		ClarifyKZ:     "Оферта сайтқа керек пе, әлде мобильді қосымшаға ма?",
		ClarifyEN:     "Is the offer for a website or a mobile application?",
	},
	{
		Code:          domain.ServicePrivacyPolicy,
		NameRU:        "Политика конфиденциальности",
		NameKZ:        "Құпиялылық саясаты",
		NameEN:        "Privacy policy",
		DescriptionRU: "Политика конфиденциальности и обработки персональных данных.",
		DescriptionKZ: "Құпиялылық және дербес деректерді өңдеу саясаты.",
		DescriptionEN: "Privacy and personal data processing policy.",
		ClarifyRU:     "Политика нужна для сайта или для мобильного приложения?",
		ClarifyKZ:     "Саясат сайтқа керек пе, әлде мобильді қосымшаға ма?",
		ClarifyEN:     "Is the policy for a website or a mobile application?",
	},
	{
		Code:          domain.ServiceWebsiteUserAgreement,
		NameRU:        "Пользовательское соглашение для сайта",
		NameKZ:        "Сайтқа арналған пайдаланушы келісімі",
		NameEN:        "Website user agreement",
		DescriptionRU: "Пользовательское соглашение и правила использования сайта.",
		DescriptionKZ: "Сайтты пайдалану ережелері мен пайдаланушы келісімі.",
		DescriptionEN: "User agreement and terms of use for a website.",
		ClarifyRU:     "Подскажите, сайт уже работает или находится в разработке?",
		ClarifyKZ:     "Сайт жұмыс істеп тұр ма, әлде әзірленуде ме?",
		ClarifyEN:     "Is the website already live or still in development?",
	},
	{
		Code:          domain.ServiceMobileAppUserAgreement,
		NameRU:        "Пользовательское соглашение для приложения",
		NameKZ:        "Қосымшаға арналған пайдаланушы келісімі",
		NameEN:        "Mobile app user agreement",
		DescriptionRU: "Пользовательское соглашение для мобильного приложения.",
		DescriptionKZ: "Мобильді қосымшаға арналған пайдаланушы келісімі.",
		DescriptionEN: "User agreement for a mobile application.",
		ClarifyRU:     "Подскажите, приложение уже запущено или находится в разработке?",
		ClarifyKZ:     "Қосымша іске қосылды ма, әлде әзірленуде ме?",
		ClarifyEN:     "Is the application already launched or still in development?",
	},
	{
		Code:          domain.ServiceEcommerceDocuments,
		NameRU:        "Документы для интернет-магазина",
		NameKZ:        "Интернет-дүкенге арналған құжаттар",
		NameEN:        "E-commerce documents",
		DescriptionRU: "Комплект документов для интернет-магазина.",
		DescriptionKZ: "Интернет-дүкенге арналған құжаттар топтамасы.",
		DescriptionEN: "Document package for an online store.",
		ClarifyRU:     "Подскажите, что вы продаёте: товары или услуги?",
		ClarifyKZ:     "Не сатасыз: тауарлар ма, әлде қызметтер ме?",
		ClarifyEN:     "What do you sell: goods or services?",
	},
	{
		Code:          domain.ServiceOnlinePlatformDocs,
		NameRU:        "Документы для онлайн-платформы",
		NameKZ:        "Онлайн-платформаға арналған құжаттар",
		NameEN:        "Online platform documents",
		DescriptionRU: "Документы для маркетплейса или онлайн-платформы.",
		DescriptionKZ: "Маркетплейс немесе онлайн-платформаға арналған құжаттар.",
		DescriptionEN: "Documents for a marketplace or online platform.",
		ClarifyRU:     "Подскажите, на платформе есть оплата между пользователями?",
		ClarifyKZ:     "Платформада пайдаланушылар арасында төлем бар ма?",
		ClarifyEN:     "Does the platform process payments between users?",
	},
	{
		Code:          domain.ServicePaymentRefundPolicy,
		NameRU:        "Политика оплаты и возврата",
		NameKZ:        "Төлем және қайтару саясаты",
		NameEN:        "Payment and refund policy",
		DescriptionRU: "Правила оплаты и возврата средств.",
		DescriptionKZ: "Төлем және қаражатты қайтару ережелері.",
		DescriptionEN: "Payment and refund rules.",
		ClarifyRU:     "Подскажите, какие способы оплаты вы используете?",
		ClarifyKZ:     "Қандай төлем әдістерін қолданасыз?",
		ClarifyEN:     "Which payment methods do you use?",
	},
	{
		Code:          domain.ServiceDeliveryPolicy,
		NameRU:        "Политика доставки",
		NameKZ:        "Жеткізу саясаты",
		NameEN:        "Delivery policy",
		DescriptionRU: "Условия и правила доставки.",
		DescriptionKZ: "Жеткізу шарттары мен ережелері.",
		DescriptionEN: "Delivery terms and rules.",
		ClarifyRU:     "Доставка осуществляется по Казахстану или за рубеж?",
		ClarifyKZ:     "Жеткізу Қазақстан бойынша ма, әлде шетелге ме?",
		ClarifyEN:     "Do you deliver within Kazakhstan or abroad?",
	},
	{
		Code:          domain.ServiceOtherLegalService,
		NameRU:        "Другая юридическая услуга",
		NameKZ:        "Басқа заңгерлік қызмет",
		NameEN:        "Other legal service",
		DescriptionRU: "Иные юридические вопросы.",
		DescriptionKZ: "Өзге де құқықтық мәселелер.",
		DescriptionEN: "Other legal matters.",
		ClarifyRU:     "Опишите, пожалуйста, кратко, что именно требуется.",
		ClarifyKZ:     "Не қажет екенін қысқаша сипаттап беріңізші.",
		ClarifyEN:     "Could you briefly describe what exactly you need?",
	},
}
