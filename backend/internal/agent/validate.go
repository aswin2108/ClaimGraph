package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxSummaryLen   = 1500
	maxFindings     = 6
	maxUncertainty  = 5
	maxTextFieldLen = 600
	maxCitations    = 12
)

// bannedLanguage lists accusatory terms the product must never output.
var bannedLanguage = regexp.MustCompile(`(?i)\b(fraudster|fraudsters|guilty|convicted|criminal|criminals|perpetrator|culprit|scammer|committed fraud|is committing fraud|proven fraud|proof of fraud|definitely fraud)\b`)

// bannedActions are automatic claim decisions the agent may never recommend.
var bannedActions = regexp.MustCompile(`(?i)\b(deny|denied|denial|reject|rejected|cancel|cancelled|terminate|terminated|void|approve|approved|release (the )?payment|withhold (the )?payment|pay out|prosecute|arrest)\b`)

var scoreMention = regexp.MustCompile(`(?i)score[^0-9.\n]{0,25}(\d{1,3})`)

// idLike matches identifiers such as CLM-1001 or DEV-01: letters, a hyphen, and a digit
// somewhere. Words like "multi-hop" or "non-public" have no digit and are ignored.
var idLike = regexp.MustCompile(`[A-Za-z]{2,}(?:-[A-Za-z0-9]+)+`)

// parseExplanation extracts the final JSON object from the model's text and decodes
// it strictly.
func parseExplanation(text string) (*Explanation, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, errors.New("reply contained no JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text[start : end+1])))
	dec.DisallowUnknownFields()
	var e Explanation
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("reply was not valid JSON for the required schema: %v", err)
	}
	return &e, nil
}

// validateExplanation checks that an LLM explanation is grounded in what the tools
// actually returned and respects the product's language rules. seen is the set of
// identifier tokens shown to the model this run; scores is the set of scores it may
// legitimately mention.
func validateExplanation(e *Explanation, seen map[string]bool, scores map[int]bool) error {
	if strings.TrimSpace(e.Summary) == "" || strings.TrimSpace(e.NextStep.Action) == "" {
		return errors.New("summary and next_step.action are required")
	}
	if len(e.Summary) > maxSummaryLen || len(e.KeyFindings) > maxFindings || len(e.Uncertainty) > maxUncertainty {
		return errors.New("reply exceeds the allowed size")
	}

	var text []string
	text = append(text, e.Summary, e.NextStep.Action, e.NextStep.Rationale)
	text = append(text, e.Uncertainty...)
	cited := append(append([]string{}, e.NextStep.ClaimIDs...), e.NextStep.Entities...)
	for _, f := range e.KeyFindings {
		text = append(text, f.Statement)
		cited = append(cited, f.ClaimIDs...)
		cited = append(cited, f.Entities...)
	}
	for _, t := range text {
		if len(t) > maxTextFieldLen && t != e.Summary {
			return errors.New("a text field is too long")
		}
	}
	if len(cited) > maxCitations*(maxFindings+1) {
		return errors.New("too many citations")
	}

	joined := strings.Join(text, "\n")
	if m := bannedLanguage.FindString(joined); m != "" {
		return fmt.Errorf("accusatory language is not allowed (%q); use investigation-oriented wording", m)
	}
	if m := bannedActions.FindString(e.NextStep.Action + " " + e.NextStep.Rationale); m != "" {
		return fmt.Errorf("the next step must be a verification action; it may not recommend %q or any automatic claim decision", m)
	}

	// Every cited id must have been shown to the model by a tool this run.
	for _, c := range cited {
		id := bareID(c)
		if !seen[id] && !seen[c] {
			return fmt.Errorf("cited id %q was not returned by any tool", clip(c, 60))
		}
	}
	// Any identifier-looking token in the free text must also be grounded.
	for _, tok := range idLike.FindAllString(joined, -1) {
		if !hasDigit(tok) {
			continue
		}
		if !seen[strings.TrimRight(tok, ".-")] {
			return fmt.Errorf("identifier %q in the text was not returned by any tool", clip(tok, 60))
		}
	}
	// Any score quoted in the text must be a score the model was actually given.
	for _, m := range scoreMention.FindAllStringSubmatch(joined, -1) {
		n, _ := strconv.Atoi(m[1])
		if !scores[n] {
			return fmt.Errorf("score %d is not a score returned by the deterministic analysis or a counterfactual", n)
		}
	}
	return nil
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}
