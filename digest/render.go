package digest

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"strconv"
)

//go:embed templates/digest.html
var files embed.FS

// signed and pct are built only from numbers, and returning them as HTML
// keeps html/template from writing their "+" as &#43;.
var digestTmpl = template.Must(template.New("digest.html").Funcs(template.FuncMap{
	"money":      money,
	"signed":     func(v float64) template.HTML { return template.HTML(signedMoney(v)) },
	"deltaClass": deltaClass,
	"pct":        func(p *float64) template.HTML { return template.HTML(pct(p)) },
	"prev":       func(l Line) string { return unlessNoComparison(l, money(l.Prev)) },
	"change":     func(l Line) template.HTML { return template.HTML(unlessNoComparison(l, signedMoney(l.Delta))) },
}).ParseFS(files, "templates/digest.html"))

// Render renders the email. footer is a plain-text note, such as where the
// provider's queries ran.
func Render(subject, footer string, sections []Section) (string, error) {
	if len(sections) == 0 {
		return "", errors.New("no sections to render")
	}
	var b bytes.Buffer
	err := digestTmpl.Execute(&b, map[string]any{"Subject": subject, "Footer": footer, "Sections": sections, "Coverage": summaryText(sections)})
	return b.String(), err
}

// money is a cost to the cent. Rounding first and dropping the sign of a
// zero keeps a sub-cent negative adjustment from printing as "-0.00".
func money(v float64) string {
	r := round2(v)
	if r == 0 {
		r = 0 // -0 becomes 0
	}
	return strconv.FormatFloat(r, 'f', 2, 64)
}

// signedMoney is money with a "+" on a real increase, decided by the value
// as displayed, so a delta that shows as 0.00 never carries one.
func signedMoney(v float64) string {
	if round2(v) > 0 {
		return "+" + money(v)
	}
	return money(v)
}

// unlessNoComparison blanks a figure measured against a month the export never
// held, since 0.00 there would claim the account spent nothing.
func unlessNoComparison(l Line, figure string) string {
	if l.NoComparison {
		return "—"
	}
	return figure
}

// deltaClass colours a change cell by the same rounded value, so colour and
// sign cannot disagree.
func deltaClass(v float64) string {
	switch r := round2(v); {
	case r > 0:
		return "delta-pos"
	case r < 0:
		return "delta-neg"
	}
	return "delta-zero"
}

// pct is a percentage cell: whole percent with a "+" on an increase, or a
// dash when there is no baseline to measure against.
func pct(p *float64) string {
	if p == nil {
		return "—"
	}
	if *p > 0 {
		return fmt.Sprintf("+%.0f%%", *p)
	}
	return fmt.Sprintf("%.0f%%", *p)
}
