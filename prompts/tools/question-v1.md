Puts a decision to the person you work for and returns their answer. One call asks 1 to 4 questions at once. Each question has a short header, the full question, and 2 to 4 options to choose from, and the person can always answer in their own words instead.

Use it when a decision is the person's to make and it changes what you do next: a requirement that has more than one reading, a trade-off between approaches, a preference that nothing on the machine settles.

Do not use it for:
- a choice that has a conventional default: take the default and say that you did;
- a fact the machine can tell you: read it;
- permission for an action: make the call, and the session's approvals decide;
- whether to continue;
- anything the person already answered or left to you.

How to ask:
- Ask everything you need in one call. A step takes one `question` call, and a second one in the same step is refused.
- Write each question so that it can be answered without reading the conversation.
- Give each option a `description` that says what follows from choosing it and what it costs.
- Put the option you would take first and mark it `recommended`.
- Add no option for "other": the person can always write their own answer.
- Set `multiple` when the person may choose several options.
- The other calls of the same step do not wait for the answer, so start nothing in that step that depends on it.

Inputs:
- `questions`: 1 to 4 questions, shown together. Each has:
  - `header`: a short label of at most 12 characters, such as a tab's title.
  - `question`: the full question, at most 300 characters.
  - `options`: 2 to 4 options, in the order they are shown. Each has:
    - `label`: the option's name, at most 40 characters, unique in its question.
    - `description`: what choosing the option means, at most 200 characters.
    - `preview`: optional, a mockup or a snippet as a list of lines of plain text, at most 15 lines of at most 40 characters each, shown in a monospace font. Add one only where seeing helps to compare the options, such as a layout, a snippet or a file tree, and only on a question that takes one option. The question must be answerable without it.
    - `recommended`: true on the option you would take.
  - `multiple`: true when the person may choose several options. The default is false.

The result lists each question with the labels the person chose and what they wrote in their own words. A question the person left to you is yours to decide: decide it and say what you assumed. When the person sent a message in place of an answer, the result says so and the message follows. It may be the answer.

When nobody attends the session, the result says so at once. Decide each question yourself, state each assumption in your final message, and do not ask again in this session.
