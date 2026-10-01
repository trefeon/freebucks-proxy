package convert

import "strings"

// tools_attribution.go — strip the vendor's commit-attribution footer from
// the served run_terminal_command definition.
//
// The stock CLI definition instructs the model to end every commit with
// "Generated with Codebuff 🤖 / Co-Authored-By: Codebuff
// <noreply@codebuff.com>". The vendor itself ships a no-attribution variant
// of the same description (runTerminalCommandNoAttributionDescription,
// selected per run off the agent definition's suppressCommitAttribution —
// upstream/freebuff/common/src/tools/params/tool/run-terminal-command.ts),
// so the stripped text below mirrors that variant word for word: step 4
// becomes the plain "Do NOT add any trailer" step and the footer example
// becomes a bare commit message. Everything else in the description is
// untouched.
//
// The fixture (testdata/cli-tools.json) stays byte-identical to the vendor
// capture — parity tests pin that — so the strip runs here at serve time,
// inside canonicalToolDefs' sync.Once (race-safe: before any reader).
// Keep in sync with upstream/commit_attribution.go (upstream may not import
// convert per the archtest matrix, so the helper is duplicated, not shared).
//
// Gate note: the free-tier gate keys on tool definitions, so a description
// change is a fingerprint change — re-prove the wire live (single probe,
// stop on 503/403) after touching this.
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

// attributionStepHead opens the footer-instructing step 4; attributionTail
// opens the section that follows it. Both must be present or the
// description is left untouched (already stripped, or a future vendor
// shape this code does not know — serve stock, never corrupt).
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

// attributionFooterExample is the second worked example's footer (literal
// backslash-n sequences, part of the example shell text).
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
