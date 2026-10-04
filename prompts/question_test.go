// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts_test

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// TestQuestionDescriptionHoldsTheBounds: the question tool's description
// states each bound of a call in the words the model reads, with the
// value of its constant, and holds no other number, so a bound that
// changes fails here until a new version of the file states it.
func TestQuestionDescriptionHoldsTheBounds(t *testing.T) {
	d := prompts.Text(prompts.ToolQuestion)
	for _, want := range []string{
		fmt.Sprintf("One call asks 1 to %d questions at once", session.MaxQuestions),
		fmt.Sprintf("and %d to %d options to choose from", session.MinQuestionOptions, session.MaxQuestionOptions),
		fmt.Sprintf("`questions`: 1 to %d questions", session.MaxQuestions),
		fmt.Sprintf("`header`: a short label of at most %d characters", session.MaxQuestionHeaderLength),
		fmt.Sprintf("`question`: the full question, at most %d characters", session.MaxQuestionLength),
		fmt.Sprintf("`options`: %d to %d options", session.MinQuestionOptions, session.MaxQuestionOptions),
		fmt.Sprintf("`label`: the option's name, at most %d characters", session.MaxOptionLabelLength),
		fmt.Sprintf("`description`: what choosing the option means, at most %d characters", session.MaxOptionDescriptionLength),
		fmt.Sprintf("at most %d lines of at most %d characters each", session.MaxOptionPreviewLines, session.MaxOptionPreviewColumns),
	} {
		if !strings.Contains(d, want) {
			t.Errorf("the description does not state %q", want)
		}
	}
	bounds := []int{1, session.MaxQuestions, session.MinQuestionOptions, session.MaxQuestionOptions, session.MaxQuestionHeaderLength, session.MaxQuestionLength,
		session.MaxOptionLabelLength, session.MaxOptionDescriptionLength, session.MaxOptionPreviewLines, session.MaxOptionPreviewColumns}
	for _, m := range regexp.MustCompile(`[0-9]+`).FindAllString(d, -1) {
		n, err := strconv.Atoi(m)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(bounds, n) {
			t.Errorf("the description holds the number %d, which is no bound of a question", n)
		}
	}
	// What the description tells the model, by the rules of spec 039's
	// table.
	for _, want := range []string{
		"a requirement that has more than one reading", "a conventional default", "permission for an action", "whether to continue",
		"already answered or left to you", "in one call", "a second one in the same step is refused", "mark it `recommended`",
		`no option for "other"`, "do not wait for the answer", "only on a question that takes one option", "answerable without it",
		"It may be the answer", "nobody attends the session", "do not ask again in this session",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("the description lacks %q", want)
		}
	}
	// The refusal of a second call names the same bound.
	if got := prompts.Render(prompts.QuestionSecondCall, prompts.Data{"Max": session.MaxQuestions}); !strings.Contains(got, fmt.Sprintf("at most %d questions", session.MaxQuestions)) {
		t.Errorf("the refusal of a second call: %s", got)
	}
}
