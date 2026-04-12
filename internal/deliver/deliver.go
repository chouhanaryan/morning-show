// Package deliver writes the generated briefing to disk and optionally emails
// it via SMTP.
package deliver

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
		// Plan says: SMTP rejection is a hard fail for email (Actions step
		// should surface it), but the report is already on disk. Return the
		// error — main decides whether to exit non-zero.
		return out, fmt.Errorf("email delivery failed: %w", err)
	}
	out.Emailed = true
	return out, nil
}

func (d *Deliverer) sendEmail(markdown string) error {
	ec := d.cfg.Deliver.Email
	if ec.From == "" || len(ec.To) == 0 {
		return fmt.Errorf("email: from and to are required")
	}
	user := os.Getenv(ec.UserEnv)
	pass := os.Getenv(ec.PassEnv)
	if user == "" || pass == "" {
		return fmt.Errorf("email: %s/%s not set", ec.UserEnv, ec.PassEnv)
	}
	msg := mail.NewMsg()
	if err := msg.From(ec.From); err != nil {
		return fmt.Errorf("set from: %w", err)
	}
	if err := msg.To(ec.To...); err != nil {
		return fmt.Errorf("set to: %w", err)
	}
	msg.Subject(fmt.Sprintf("Weekly Briefing — %s", time.Now().UTC().Format("2006-01-02")))
	msg.SetBodyString(mail.TypeTextPlain, markdown)

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

// UsageSummary is a pipeline-agnostic view of token totals for the footer.
type UsageSummary struct {
	Pass1In, Pass1Out int
	Pass2In, Pass2Out int
	Pass3In, Pass3Out int
	Pass1Batches      int
	Pass2Batches      int
	Pass3Batches      int
	FeedsReached      int
	FeedsTotal        int
	Provider          string
	Model             string
	Duration          time.Duration
}

// Markdown renders the footer block.
func (u UsageSummary) Markdown() string {
	return fmt.Sprintf(`## Run Metadata

- Provider: %s
- Model: %s
- Duration: %s
- Feeds reached: %d/%d

### Token usage

| Pass | Batches | Input | Output |
|------|---------|-------|--------|
| 1 (score)      | %d | %d | %d |
| 2 (extract)    | %d | %d | %d |
| 3 (synthesize) | %d | %d | %d |
| **Total**      |    | **%d** | **%d** |
`,
		u.Provider, u.Model, u.Duration.Round(time.Second),
		u.FeedsReached, u.FeedsTotal,
		u.Pass1Batches, u.Pass1In, u.Pass1Out,
		u.Pass2Batches, u.Pass2In, u.Pass2Out,
		u.Pass3Batches, u.Pass3In, u.Pass3Out,
		u.Pass1In+u.Pass2In+u.Pass3In,
		u.Pass1Out+u.Pass2Out+u.Pass3Out,
	)
}
