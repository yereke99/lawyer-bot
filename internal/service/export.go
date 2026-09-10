package service

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"lawyer-bot/internal/domain"
	"lawyer-bot/internal/repository"
)

// ExportService renders a filtered CRM client list as CSV or XLSX.
//
// Field selection is a deliberate allow-list built in this file. Password
// hashes, session tokens, provider credentials and OpenAI keys are not in the
// client model at all, and nothing here reads from a table that holds them, so
// an export cannot leak a secret even if a new column is added elsewhere.
type ExportService struct {
	clients *repository.CRMRepository
	catalog *Catalog
}

// NewExportService builds the export service.
func NewExportService(clients *repository.CRMRepository, catalog *Catalog) *ExportService {
	return &ExportService{clients: clients, catalog: catalog}
}

// exportColumns is the exported field set, with a localised header per column.
var exportColumns = []struct {
	KK string
	RU string
	EN string
}{
	{"Клиент", "Клиент", "Client"},
	{"Телефон", "Телефон", "Phone"},
	{"Тіл", "Язык", "Language"},
	{"Қызмет", "Услуга", "Service"},
	{"Мәртебе", "Статус", "Status"},
	{"Режим", "Режим", "Mode"},
	{"Кеңесші", "Консультант", "Consultant"},
	{"Бірінші байланыс", "Первый контакт", "First contact"},
	{"Соңғы байланыс", "Последний контакт", "Last contact"},
	{"Кезең", "Этап", "Stage"},
	{"Қысқаша мазмұны", "Краткое резюме", "Summary"},
	{"Келесі қадам", "Следующий шаг", "Next action"},
	{"Келесі еске салу", "Следующее напоминание", "Next follow-up"},
	{"Тегтер", "Теги", "Tags"},
	{"Нәтиже", "Результат", "Result"},
}

func header(lang domain.Language) []string {
	out := make([]string, 0, len(exportColumns))
	for _, c := range exportColumns {
		switch lang.OrDefault() {
		case domain.LangKK:
			out = append(out, c.KK)
		case domain.LangEN:
			out = append(out, c.EN)
		default:
			out = append(out, c.RU)
		}
	}
	return out
}

// maxExportRows caps one export, so a click can never build an unbounded file.
const maxExportRows = 20000

// Rows renders the filtered clients as a header plus data rows.
func (s *ExportService) Rows(ctx context.Context, filter repository.ClientFilter, lang domain.Language) ([][]string, error) {
	filter.Limit = 200
	filter.Offset = 0

	rows := [][]string{header(lang)}
	for len(rows) <= maxExportRows {
		page, _, err := s.clients.ListClients(ctx, filter)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			rows = append(rows, s.row(&page[i], lang))
		}
		if len(page) < filter.Limit {
			break
		}
		filter.Offset += filter.Limit
	}
	return rows, nil
}

func (s *ExportService) row(c *domain.CRMClient, lang domain.Language) []string {
	name := strings.TrimSpace(c.DisplayName)
	if name == "" {
		name = "—"
	}
	service := "—"
	if c.DetectedService != "" {
		service = s.catalog.Name(c.DetectedService, lang)
	}
	consultant := c.AssignedAdminName
	if consultant == "" {
		consultant = "—"
	}
	result := c.CloseReason
	if result == "" {
		result = "—"
	}
	return []string{
		name,
		FormatE164(c.PhoneNumber),
		strings.ToUpper(string(c.Language.OrDefault())),
		service,
		StatusLabel(c.CRMStatus, lang),
		ModeLabel(c.Mode, lang),
		consultant,
		formatTime(&c.User.CreatedAt),
		formatTime(latest(c.LastInboundAt, c.LastOutboundAt)),
		stageLabel(c.QualificationStage, lang),
		c.AISummary,
		c.NextAction,
		formatTime(c.NextFollowUpAt),
		strings.Join(c.Tags, ", "),
		result,
	}
}

// WriteCSV renders rows as CSV.
//
// The UTF-8 byte order mark is what makes Excel on Windows open Cyrillic and
// Kazakh headers correctly instead of showing mojibake.
func WriteCSV(w io.Writer, rows [][]string) error {
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return fmt.Errorf("write csv bom: %w", err)
	}
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	for _, row := range rows {
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteXLSX renders rows as a minimal but valid .xlsx workbook.
//
// It is written by hand against the SpreadsheetML schema rather than pulling in
// a spreadsheet library: the CRM needs one sheet of inline strings, and a new
// third-party dependency for that would not pay for itself.
func WriteXLSX(w io.Writer, rows [][]string, sheetName string) error {
	if sheetName == "" {
		sheetName = "CRM"
	}
	zw := zip.NewWriter(w)

	files := []struct {
		name    string
		content string
	}{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", rootRelsXML},
		{"xl/workbook.xml", fmt.Sprintf(workbookXML, xmlEscape(sheetName))},
		{"xl/_rels/workbook.xml.rels", workbookRelsXML},
		{"xl/styles.xml", stylesXML},
		{"xl/worksheets/sheet1.xml", sheetXML(rows)},
	}
	for _, f := range files {
		part, err := zw.Create(f.name)
		if err != nil {
			return fmt.Errorf("create xlsx part %s: %w", f.name, err)
		}
		if _, err := io.WriteString(part, f.content); err != nil {
			return fmt.Errorf("write xlsx part %s: %w", f.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finalise xlsx: %w", err)
	}
	return nil
}

func sheetXML(rows [][]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	b.WriteString(`<sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, r+1)
		for c, value := range row {
			style := ""
			if r == 0 {
				style = ` s="1"`
			}
			fmt.Fprintf(&b, `<c r="%s%d" t="inlineStr"%s><is><t xml:space="preserve">%s</t></is></c>`,
				columnName(c), r+1, style, xmlEscape(value))
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// columnName renders a zero-based column index as A, B, ... Z, AA, AB.
func columnName(index int) string {
	name := ""
	for index >= 0 {
		name = string(rune('A'+index%26)) + name
		index = index/26 - 1
	}
	return name
}

func xmlEscape(s string) string {
	// Strip control characters: they are illegal in XML 1.0 and would produce a
	// workbook Excel refuses to open.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, s)
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
</Types>`

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

const workbookXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="%s" sheetId="1" r:id="rId1"/></sheets>
</workbook>`

const workbookRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`

const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>
<fills count="1"><fill><patternFill patternType="none"/></fill></fills>
<borders count="1"><border/></borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs>
</styleSheet>`

// ------------------------------------------------------------------ helpers

func formatTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func latest(times ...*time.Time) *time.Time {
	var out *time.Time
	for _, t := range times {
		if t == nil || t.IsZero() {
			continue
		}
		if out == nil || t.After(*out) {
			out = t
		}
	}
	return out
}

func stageLabel(stage string, lang domain.Language) string {
	table, ok := stageLabels[stage]
	if !ok {
		return "—"
	}
	return tr(lang, table)
}

var stageLabels = map[string]map[domain.Language]string{
	domain.StageLanguage:       {domain.LangRU: "Язык определён", domain.LangKK: "Тіл анықталды", domain.LangEN: "Language detected"},
	domain.StageIntent:         {domain.LangRU: "Запрос определён", domain.LangKK: "Сұраныс анықталды", domain.LangEN: "Intent detected"},
	domain.StageQualifying:     {domain.LangRU: "Уточнение", domain.LangKK: "Нақтылау", domain.LangEN: "Qualifying"},
	domain.StageQualified:      {domain.LangRU: "Квалифицирован", domain.LangKK: "Біліктілігі расталған", domain.LangEN: "Qualified"},
	domain.StageWaitingClient:  {domain.LangRU: "Ждём клиента", domain.LangKK: "Клиентті күтудеміз", domain.LangEN: "Waiting for client"},
	domain.StageConsultantReq:  {domain.LangRU: "Нужен консультант", domain.LangKK: "Кеңесші қажет", domain.LangEN: "Consultant required"},
	domain.StageHumanTakeover:  {domain.LangRU: "Ведёт консультант", domain.LangKK: "Кеңесші жүргізуде", domain.LangEN: "Human takeover"},
	domain.StageConversationOK: {domain.LangRU: "Завершено", domain.LangKK: "Аяқталды", domain.LangEN: "Converted"},
}

// ExportFileName builds a dated file name for a download.
func ExportFileName(format string) string {
	stamp := time.Now().UTC().Format("2006-01-02")
	switch strings.ToLower(format) {
	case "xlsx":
		return "crm-clients-" + stamp + ".xlsx"
	default:
		return "crm-clients-" + stamp + ".csv"
	}
}

// ParseInt is a small helper for query parameters with a default.
func ParseInt(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}
