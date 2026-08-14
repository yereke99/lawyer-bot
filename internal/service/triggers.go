package service

import (
	"sort"
	"strings"
	"unicode"
)

// Trigger is one configurable category of deterministic phrase matching.
//
// Triggers are only the first filter. They exist to answer one cheap question
// before any token is spent: "could this message plausibly be about legal
// services?". Semantic understanding is the model's job.
type Trigger struct {
	Code     string
	Language string
	// Phrases are matched as substrings of the normalised text. Use stems
	// ("товарн", "тауар белг") so inflected forms match without a morphology
	// engine.
	Phrases []string
	// Words are matched as whole words. Use for short tokens ("тоо", "ип")
	// that would produce false positives as substrings.
	Words []string
}

// Trigger category codes.
const (
	TriggerServiceInquiry  = "service_inquiry"
	TriggerConsultation    = "consultation_request"
	TriggerLegalAssistance = "legal_assistance"
	TriggerTrademark       = "trademark"
	TriggerRegistration    = "registration"
	TriggerContracts       = "contracts"
	TriggerPrivacyPolicy   = "privacy_policy"
	TriggerPublicOffer     = "public_offer"
	TriggerMobileApp       = "mobile_app"
	TriggerWebsiteDocs     = "website_documents"
	TriggerEcommerce       = "ecommerce"
	TriggerPricing         = "pricing"
	TriggerCallback        = "callback"
)

// TriggerMatch is the outcome of deterministic matching.
type TriggerMatch struct {
	Matched bool
	Codes   []string
	Phrases []string
}

// Has reports whether a specific category matched.
func (m TriggerMatch) Has(code string) bool {
	for _, c := range m.Codes {
		if c == code {
			return true
		}
	}
	return false
}

// TriggerSet matches text against the configured triggers.
type TriggerSet struct {
	triggers  []Trigger
	offTopic  []string
	smallTalk []string
}

// NewTriggerSet builds the default trigger set.
func NewTriggerSet() *TriggerSet {
	return &TriggerSet{
		triggers:  defaultTriggers,
		offTopic:  offTopicPhrases,
		smallTalk: smallTalkPhrases,
	}
}

// Add registers an extra trigger category at runtime.
func (t *TriggerSet) Add(tr Trigger) {
	t.triggers = append(t.triggers, tr)
}

// Match runs deterministic matching over the message text.
func (t *TriggerSet) Match(text string) TriggerMatch {
	norm := Normalize(text)
	if norm == "" {
		return TriggerMatch{}
	}
	padded := " " + norm + " "

	seen := make(map[string]bool)
	var codes, phrases []string

	for _, tr := range t.triggers {
		hit := ""
		for _, p := range tr.Phrases {
			if p != "" && strings.Contains(norm, p) {
				hit = p
				break
			}
		}
		if hit == "" {
			for _, w := range tr.Words {
				if w != "" && strings.Contains(padded, " "+w+" ") {
					hit = w
					break
				}
			}
		}
		if hit == "" {
			continue
		}
		if !seen[tr.Code] {
			seen[tr.Code] = true
			codes = append(codes, tr.Code)
		}
		phrases = append(phrases, hit)
	}

	sort.Strings(codes)
	return TriggerMatch{Matched: len(codes) > 0, Codes: codes, Phrases: phrases}
}

// IsOffTopic reports whether the text is recognisably unrelated to legal
// services: weather, small talk, chit-chat. Used to keep such messages away
// from the model entirely.
func (t *TriggerSet) IsOffTopic(text string) bool {
	norm := Normalize(text)
	if norm == "" {
		return false
	}
	for _, p := range t.offTopic {
		if strings.Contains(norm, p) {
			return true
		}
	}
	return false
}

// IsSmallTalkOnly reports whether the text consists of nothing but greetings,
// thanks and filler. "Здравствуйте" alone is small talk; "Здравствуйте, нужен
// юрист" is not, because a non-filler token remains.
func (t *TriggerSet) IsSmallTalkOnly(text string) bool {
	norm := Normalize(text)
	if norm == "" {
		return false
	}
	remaining := norm
	for _, p := range t.smallTalk {
		remaining = strings.ReplaceAll(remaining, p, " ")
	}
	for _, f := range fillerWords {
		remaining = strings.ReplaceAll(" "+remaining+" ", " "+f+" ", " ")
	}
	return strings.TrimSpace(remaining) == ""
}

// Normalize lowercases the text, folds "ё" to "е" and reduces every character
// that is not a letter or digit to a single space. Matching then works the same
// way for "Какие услуги?" and "какие  услуги!!!".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == 'ё':
			r = 'е'
		case r == 'ѐ':
			r = 'е'
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteRune(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// fillerWords are tokens that carry no intent on their own.
var fillerWords = []string{
	"а", "и", "но", "ну", "вот", "ок", "окей", "да", "нет", "пожалуйста",
	"ага", "угу", "ясно",
	"the", "a", "ok", "okay", "yes", "no", "please",
	"иә", "жоқ", "жақсы",
}

// smallTalkPhrases are greetings and pleasantries. Alone they never justify a
// reply: the bot must not answer "Здравствуйте" with a sales pitch.
var smallTalkPhrases = []string{
	// Russian
	"здравствуйте", "здравствуй", "доброе утро", "добрый день", "добрый вечер",
	"привет", "приветствую", "салам", "спасибо", "благодарю", "до свидания",
	"пока", "хорошо", "понятно",
	// Kazakh
	"сәлеметсіз бе", "сәлеметсізбе", "сәлем", "салеметсиз бе", "қайырлы таң",
	"қайырлы күн", "қайырлы кеш", "рахмет", "сау болыңыз", "жарайды", "түсінікті",
	// English
	"hello", "hi there", "hi", "hey", "good morning", "good afternoon",
	"good evening", "thanks", "thank you", "bye", "goodbye", "got it",
}

// offTopicPhrases are explicit non-legal subjects. A message containing one of
// these and no legal trigger is dropped before the model is called.
var offTopicPhrases = []string{
	// Russian
	"как дела", "как ты", "как вы поживаете", "что делаешь", "чем занимаешься",
	"какая погода", "погода", "который час", "сколько времени", "анекдот",
	"расскажи шутку", "футбол", "новости", "как настроение", "что нового",
	"кто ты", "ты бот", "ты робот", "ты человек",
	// Kazakh
	"қалың қалай", "қалыңыз қалай", "не істеп жатырсың", "ауа райы",
	"сағат неше", "жаңалық", "сен ботсың ба", "кімсің",
	// English
	"how are you", "how r u", "what are you doing", "weather", "what time is it",
	"tell me a joke", "football", "news", "who are you", "are you a bot",
	"are you human",
}

// defaultTriggers is the shipped deterministic filter. Phrases are stems, so
// inflected and mixed-language forms are covered without an exhaustive list.
var defaultTriggers = []Trigger{
	{
		Code:     TriggerServiceInquiry,
		Language: "ru",
		Phrases: []string{
			"чем можете помочь", "чем вы можете помочь", "какие услуги",
			"какие у вас услуг", "ваши услуг", "оказыва", "услуг", "прайс",
			"что вы делаете", "какие направлен",
		},
	},
	{
		Code:     TriggerServiceInquiry,
		Language: "kk",
		Phrases: []string{
			"қалай көмектесе", "қандай қызмет", "қызметтер", "қызметтеріңіз",
			"қызмет көрсет", "кызмет",
		},
	},
	{
		Code:     TriggerServiceInquiry,
		Language: "en",
		Phrases: []string{
			"what services", "your services", "what do you offer", "service list",
			"price list", "how can you help",
		},
	},
	{
		Code:     TriggerConsultation,
		Language: "ru",
		Phrases:  []string{"консультац", "нужен совет", "проконсультир", "вопрос по закон"},
	},
	{
		Code:     TriggerConsultation,
		Language: "kk",
		Phrases:  []string{"кеңес", "кенес керек", "консультац"},
	},
	{
		Code:     TriggerConsultation,
		Language: "en",
		Phrases:  []string{"consultation", "legal advice", "need advice"},
	},
	{
		Code:     TriggerLegalAssistance,
		Language: "ru",
		Phrases: []string{
			"юрист", "юридическ", "адвокат", "правов", "помощь юрист",
			"нужна помощь с документ", "судебн", "иск", "претенз",
		},
	},
	{
		Code:     TriggerLegalAssistance,
		Language: "kk",
		Phrases: []string{
			"заңгер", "занге", "заңды", "заң көмег", "адвокат", "құқықтық",
			"сот", "талап арыз",
		},
	},
	{
		Code:     TriggerLegalAssistance,
		Language: "en",
		Phrases:  []string{"lawyer", "legal", "attorney", "law firm", "lawsuit", "claim"},
	},
	{
		Code:     TriggerTrademark,
		Language: "ru",
		Phrases:  []string{"товарн", "торговая марка", "торгов марк", "бренд", "патент", "интеллектуальн"},
	},
	{
		Code:     TriggerTrademark,
		Language: "kk",
		Phrases:  []string{"тауар белг", "сауда белг", "патент", "зияткерлік"},
	},
	{
		Code:     TriggerTrademark,
		Language: "en",
		Phrases:  []string{"trademark", "trade mark", "brand registration", "patent", "intellectual property"},
	},
	{
		Code:     TriggerRegistration,
		Language: "ru",
		Phrases:  []string{"регистрац", "зарегистрир", "открыть бизнес", "открыть компан", "учредительн"},
		Words:    []string{"ип", "тоо", "оао", "ао"},
	},
	{
		Code:     TriggerRegistration,
		Language: "kk",
		Phrases:  []string{"тіркеу", "тіркеп", "тиркеу", "бизнес ашу", "компания ашу", "құрылтай"},
		Words:    []string{"жк", "жшс"},
	},
	{
		Code:     TriggerRegistration,
		Language: "en",
		Phrases:  []string{"registration", "register a company", "register business", "incorporat"},
		Words:    []string{"llc", "llp"},
	},
	{
		Code:     TriggerContracts,
		Language: "ru",
		Phrases:  []string{"договор", "контракт", "соглашен", "проверить документ", "составить документ"},
	},
	{
		Code:     TriggerContracts,
		Language: "kk",
		Phrases:  []string{"шарт", "келісім", "келисим", "құжат"},
	},
	{
		Code:     TriggerContracts,
		Language: "en",
		Phrases:  []string{"contract", "agreement", "nda", "document review"},
	},
	{
		Code:     TriggerPrivacyPolicy,
		Language: "ru",
		Phrases:  []string{"конфиденциальн", "персональн данн", "политик обработ", "gdpr"},
	},
	{
		Code:     TriggerPrivacyPolicy,
		Language: "kk",
		Phrases:  []string{"құпиялылық", "дербес дерек", "купиялылык"},
	},
	{
		Code:     TriggerPrivacyPolicy,
		Language: "en",
		Phrases:  []string{"privacy policy", "personal data", "gdpr", "data protection"},
	},
	{
		Code:     TriggerPublicOffer,
		Language: "ru",
		Phrases:  []string{"оферт", "публичн предложен"},
	},
	{
		Code:     TriggerPublicOffer,
		Language: "kk",
		Phrases:  []string{"оферта", "жария ұсыныс"},
	},
	{
		Code:     TriggerPublicOffer,
		Language: "en",
		Phrases:  []string{"public offer", "terms of service", "terms and conditions"},
	},
	{
		Code:     TriggerMobileApp,
		Language: "ru",
		Phrases:  []string{"мобильн приложен", "приложен", "андроид", "апстор", "гугл плей", "плейсторе"},
	},
	{
		Code:     TriggerMobileApp,
		Language: "kk",
		Phrases:  []string{"мобильді қосымша", "қосымша", "косымша"},
	},
	{
		Code:     TriggerMobileApp,
		Language: "en",
		Phrases:  []string{"mobile app", "application", "android", "app store", "google play", "ios app"},
	},
	{
		Code:     TriggerWebsiteDocs,
		Language: "ru",
		Phrases:  []string{"для сайта", "сайт", "пользовательск соглашен", "лендинг"},
	},
	{
		Code:     TriggerWebsiteDocs,
		Language: "kk",
		Phrases:  []string{"сайт", "пайдаланушы келісім"},
	},
	{
		Code:     TriggerWebsiteDocs,
		Language: "en",
		Phrases:  []string{"website", "web site", "user agreement", "landing page"},
	},
	{
		Code:     TriggerEcommerce,
		Language: "ru",
		Phrases:  []string{"интернет магазин", "маркетплейс", "онлайн магазин", "доставк", "возврат средств", "эквайринг"},
	},
	{
		Code:     TriggerEcommerce,
		Language: "kk",
		Phrases:  []string{"интернет дүкен", "маркетплейс", "жеткізу", "қайтару"},
	},
	{
		Code:     TriggerEcommerce,
		Language: "en",
		Phrases:  []string{"online store", "ecommerce", "e commerce", "marketplace", "refund policy", "delivery policy"},
	},
	{
		Code:     TriggerPricing,
		Language: "ru",
		Phrases:  []string{"сколько стоит", "стоимость", "какая цена", "цена", "тариф", "во сколько обойдет"},
	},
	{
		Code:     TriggerPricing,
		Language: "kk",
		Phrases:  []string{"қанша тұрады", "бағасы", "құны", "канша турады"},
	},
	{
		Code:     TriggerPricing,
		Language: "en",
		Phrases:  []string{"how much", "what is the price", "cost of", "pricing"},
	},
	{
		Code:     TriggerCallback,
		Language: "ru",
		Phrases:  []string{"перезвон", "свяжитесь со мной", "мой номер", "позвоните"},
	},
	{
		Code:     TriggerCallback,
		Language: "kk",
		Phrases:  []string{"хабарласыңыз", "қоңырау шал", "менің нөмір"},
	},
	{
		Code:     TriggerCallback,
		Language: "en",
		Phrases:  []string{"call me", "contact me", "my number", "call back"},
	},
}
