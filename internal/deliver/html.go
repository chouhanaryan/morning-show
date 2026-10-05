package deliver

import (
	"bytes"
	"fmt"
	"html"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md renders GitHub-flavored markdown (tables, autolinks, strikethrough).
// Raw HTML in the input is escaped, which is what we want for LLM output.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// emailCSS is kept small and conservative; Gmail and Apple Mail honor a
// <style> block in <head>.
const emailCSS = `
body { margin: 0; padding: 0; background: #f6f6f4; }
.wrap { max-width: 680px; margin: 0 auto; padding: 24px 20px; background: #ffffff;
  font: 15px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; color: #1f2328; }
h1 { font-size: 22px; margin: 0 0 16px; }
h2 { font-size: 18px; margin: 28px 0 10px; padding-bottom: 4px; border-bottom: 1px solid #e5e5e0; }
h3 { font-size: 15px; margin: 20px 0 8px; }
p, ul { margin: 0 0 12px; }
li { margin: 0 0 4px; }
a { color: #0b62c4; }
em { color: #444; }
hr { border: 0; border-top: 1px solid #e5e5e0; margin: 24px 0; }
table { border-collapse: collapse; font-size: 13px; }
th, td { border: 1px solid #e5e5e0; padding: 4px 8px; text-align: left; }
`

// RenderHTML converts the briefing markdown into a standalone HTML email.
func RenderHTML(title, markdown string) (string, error) {
	var body bytes.Buffer
	if err := md.Convert([]byte(markdown), &body); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title><style>%s</style></head>
<body><div class="wrap">
%s</div></body></html>
`, html.EscapeString(title), emailCSS, body.String()), nil
}
