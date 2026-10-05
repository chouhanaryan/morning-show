// Package deliver writes the generated briefing to disk and optionally emails
// it via SMTP.
package deliver

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chouhanaryan/morning-show/internal/config"
	"github.com/wneessen/go-mail"
)

// Delivery is the outcome of delivering a briefing.
type Delivery struct {
	ReportPath string
	Emailed    bool
}

// Deliverer writes reports and (optionally) sends email.
type Deliverer struct {
	cfg *config.Config
	log *slog.Logger
}

// New builds a Deliverer.
func New(cfg *config.Config, log *slog.Logger) *Deliverer {
	return &Deliverer{cfg: cfg, log: log}
}

// Deliver writes the markdown to disk and, if email is enabled and not dry
// run, sends it. A disk write failure is a hard error; an email failure is
// logged but the report path is still returned.
func (d *Deliverer) Deliver(md string, usage UsageSummary, dryRun bool) (*Delivery, error) {
	if err := os.MkdirAll(d.cfg.Deliver.ReportsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir reports: %w", err)
	}
	filename := fmt.Sprintf("%s.md", time.Now().UTC().Format("2006-01-02"))
	path := filepath.Join(d.cfg.Deliver.ReportsDir, filename)

	// Append a token-usage footer that doesn't depend on the LLM.
	full := md + "\n\n---\n\n" + usage.Markdown()

	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		return nil, fmt.Errorf("write report: %w", err)
	}
	d.log.Info("report written", "path", path, "bytes", len(full))

	out := &Delivery{ReportPath: path}
	if dryRun || !d.cfg.Deliver.Email.Enabled {
		return out, nil
	}
	if err := d.sendEmail(full); err != nil {
		return out, fmt.Errorf("email delivery failed: %w", err)
	}
	out.Emailed = true
	return out, nil
}

func (d *Deliverer) sendEmail(markdown string) error {
	ec := d.cfg.Deliver.Email
	from := os.Getenv(ec.FromEnv)
	toRaw := os.Getenv(ec.ToEnv)
	if from == "" || toRaw == "" {
		return fmt.Errorf("email: %s/%s not set", ec.FromEnv, ec.ToEnv)
	}
	to := splitAddresses(toRaw)
	if len(to) == 0 {
		return fmt.Errorf("email: %s contains no valid addresses", ec.ToEnv)
	}
	user := os.Getenv(ec.UserEnv)
	pass := os.Getenv(ec.PassEnv)
	if user == "" || pass == "" {
		return fmt.Errorf("email: %s/%s not set", ec.UserEnv, ec.PassEnv)
	}
	msg := mail.NewMsg()
	if err := msg.From(from); err != nil {
		return fmt.Errorf("set from: %w", err)
	}
	if err := msg.To(to...); err != nil {
		return fmt.Errorf("set to: %w", err)
	}
	subject := fmt.Sprintf("Weekly Briefing \u2014 %s", time.Now().UTC().Format("2006-01-02"))
	msg.Subject(subject)
	// multipart/alternative: markdown as the plain-text part, rendered HTML
	// as the preferred part.
	msg.SetBodyString(mail.TypeTextPlain, markdown)
	htmlBody, err := RenderHTML(subject, markdown)
	if err != nil {
		return err
	}
	msg.AddAlternativeString(mail.TypeTextHTML, htmlBody)

	client, err := mail.NewClient(ec.SMTPHost,
		mail.WithPort(ec.SMTPPort),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithUsername(user),
		mail.WithPassword(pass),
		mail.WithTLSPortPolicy(mail.TLSMandatory),
	)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	if err := client.DialAndSend(msg); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

// PassUsage is one row of the token-usage footer.
type PassUsage struct {
	Name    string // e.g. "1 (score)"
	Model   string
	Batches int
	Failed  int // batches that failed and were skipped
	In, Out int
}

// cost returns the estimated USD cost of this pass, and false when the
// model has no configured price.
func (p PassUsage) cost(pricing map[string]config.ModelPrice) (float64, bool) {
	price, ok := pricing[p.Model]
	if !ok {
		return 0, false
	}
	return (float64(p.In)*price.Input + float64(p.Out)*price.Output) / 1e6, true
}

// UsageSummary is a pipeline-agnostic view of token totals for the footer.
type UsageSummary struct {
	Passes       []PassUsage
	FeedsReached int
	FeedsTotal   int
	FailedFeeds  []string
	Provider     string
	Model        string
	Pass1Model   string
	Pass2Model   string
	Duration     time.Duration
	Pricing      map[string]config.ModelPrice
	// Quality checks on the briefing text.
	RemovedLinks    int
	MissingSections []string
	Pass3Retried    bool
	// SourceHealth lists sources needing attention, as "Name: issue".
	SourceHealth []string
}

// Markdown renders the footer block.
func (u UsageSummary) Markdown() string {
	var b strings.Builder
	b.WriteString("## Run Metadata\n\n")
	fmt.Fprintf(&b, "- Provider: %s\n", u.Provider)
	fmt.Fprintf(&b, "- Model: %s\n", u.Model)
	switch {
	case u.Pass1Model == u.Pass2Model && u.Pass1Model != u.Model:
		fmt.Fprintf(&b, "- Pass 1/2/4 model: %s\n", u.Pass1Model)
	default:
		if u.Pass1Model != u.Model {
			fmt.Fprintf(&b, "- Pass 1 model: %s\n", u.Pass1Model)
		}
		if u.Pass2Model != u.Model {
			fmt.Fprintf(&b, "- Pass 2/4 model: %s\n", u.Pass2Model)
		}
	}
	fmt.Fprintf(&b, "- Duration: %s\n", u.Duration.Round(time.Second))
	fmt.Fprintf(&b, "- Feeds reached: %d/%d\n", u.FeedsReached, u.FeedsTotal)
	if len(u.FailedFeeds) > 0 {
		fmt.Fprintf(&b, "- Feeds failed: %s\n", strings.Join(u.FailedFeeds, ", "))
	}
	var checks []string
	if u.Pass3Retried {
		checks = append(checks, "synthesis retried once")
	}
	if u.RemovedLinks > 0 {
		checks = append(checks, fmt.Sprintf("%d unverifiable link(s) removed", u.RemovedLinks))
	}
	if len(u.MissingSections) > 0 {
		checks = append(checks, "missing sections: "+strings.Join(u.MissingSections, ", "))
	}
	if len(checks) > 0 {
		fmt.Fprintf(&b, "- Quality checks: %s\n", strings.Join(checks, "; "))
	} else {
		b.WriteString("- Quality checks: passed\n")
	}

	if len(u.SourceHealth) > 0 {
		b.WriteString("\n### Source health\n\n")
		for _, line := range u.SourceHealth {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}

	b.WriteString("\n### Token usage\n\n")
	b.WriteString("| Pass | Batches | Input | Output | Est. cost |\n")
	b.WriteString("|------|---------|-------|--------|-----------|\n")
	var totalIn, totalOut int
	var totalCost float64
	allPriced := true
	for _, p := range u.Passes {
		batches := fmt.Sprintf("%d", p.Batches)
		if p.Failed > 0 {
			batches += fmt.Sprintf(" (%d failed)", p.Failed)
		}
		costCell := "n/a"
		if c, ok := p.cost(u.Pricing); ok {
			costCell = fmt.Sprintf("$%.2f", c)
			totalCost += c
		} else if p.In+p.Out > 0 {
			allPriced = false
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", p.Name, batches, p.In, p.Out, costCell)
		totalIn += p.In
		totalOut += p.Out
	}
	totalCell := fmt.Sprintf("**$%.2f**", totalCost)
	if !allPriced {
		totalCell += " (partial)"
	}
	fmt.Fprintf(&b, "| **Total** | | **%d** | **%d** | %s |\n", totalIn, totalOut, totalCell)
	b.WriteString("\nCost is an estimate from list prices in `llm.pricing`; it ignores refusal-fallback reruns.\n")
	return b.String()
}

// splitAddresses splits a comma-separated list of email addresses,
// trimming whitespace from each entry and skipping empty strings.
func splitAddresses(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
