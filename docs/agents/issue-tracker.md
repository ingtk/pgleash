# Issue tracker: GitHub

Issues and specs live in GitHub Issues for `ingtk/pgleash`.
Use the `gh` CLI from this repository.

## Operations

- Create: `gh issue create --title "..." --body-file <file>`
- Read: `gh issue view <number> --json number,title,body,labels,comments`
- List: `gh issue list --state open --json number,title,body,labels`
- Comment: `gh issue comment <number> --body-file <file>`
- Label: `gh issue edit <number> --add-label "<label>"`
- Remove label: `gh issue edit <number> --remove-label "<label>"`
- Close: `gh issue close <number> --comment "..."`

Use a temporary UTF-8 file for multiline bodies and comments.

## Pull requests as a triage surface

PRs as a request surface: no.

## Skill conventions

When a skill says "publish to the issue tracker", create a GitHub issue.
When a skill says "fetch the relevant ticket", read the GitHub issue.
