package upstream

import "strings"

// commit_attribution.go — strip the vendor's commit-attribution footer from
// the served run_terminal_command definition. Duplicate of
// convert/tools_attribution.go (upstream may not import convert per the
// archtest matrix): keep the two in sync. See that file for the full
// rationale, vendor source, and gate warning.
func stripCommitAttribution(tools []any) {
	for _, t := range tools {
		m, _ := t.(map[string]any)
		if m == nil {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil || fn["name"] != "run_terminal_command" {
			continue
		}
		desc, _ := fn["description"].(string)
		if desc == "" {
			continue
		}
		fn["description"] = noAttributionDescription(desc)
	}
}

const attributionStepHead = "4. **Create the commit, ending with this specific footer:**"

const attributionTailAnchor = "\n**Important details**"

// attributionPlainStep is the vendor's own GIT_COMMIT_PLAIN_STEP, verbatim.
const attributionPlainStep = `4. **Create the commit.** Do NOT add any trailer, footer, co-author line or attribution of any kind to the commit message — no ` + "`Co-Authored-By`" + `, no "Generated with" line. The message is the message and nothing else.
   Commands run in bash on every OS (Git Bash on Windows), so always use HEREDOC syntax to format the message:
   ` + "```" + `
   git commit -m "$(cat <<'EOF'
   Your commit message here.
   EOF
   )"
   ` + "```" + `

`

const attributionFooterExample = `Your commit message here.\n\n🤖 Generated with Codebuff\nCo-Authored-By: Codebuff <noreply@codebuff.com>`

func noAttributionDescription(desc string) string {
	head := strings.Index(desc, attributionStepHead)
	tail := strings.Index(desc, attributionTailAnchor)
	if head < 0 || tail < 0 || tail <= head {
		return desc
	}
	desc = desc[:head] + attributionPlainStep + desc[tail+1:]
	return strings.Replace(desc, attributionFooterExample, "Your commit message here.", 1)
}
