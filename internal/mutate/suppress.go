package mutate

import (
	"go/ast"
	"go/token"
	"strings"
)

// directive is the comment text that suppresses mutation, with an optional
// ":reason" suffix: `//nomutant` or `//nomutant: flaky under -race`.
const directive = "nomutant"

// suppressions maps a source line to the reason recorded on the //nomutant
// directive found there. A line present in the map is suppressed; its value is
// the reason, which is the empty string for a bare `//nomutant`.
//
// A line is the whole unit of granularity here — the same unit golangci-lint's
// //nolint and staticcheck's //lint:ignore use — because a `//` comment runs to
// the end of its line, so at most one directive can ever exist per line.
type suppressions map[int]string

// scanSuppressions collects every //nomutant directive in file, keyed by the
// line the comment sits on.
//
// It walks file.Comments directly rather than building an [go/ast.CommentMap].
// A CommentMap answers "which node does this comment belong to", which sounds
// like exactly the question being asked, but its association rules have
// documented gaps around trailing and end-of-file comments (golang/go#21755,
// golang/go#33451) that would silently drop directives. Line numbers have no
// such ambiguity: the comment is on a line, the node starts and ends on lines,
// and the anchoring rule is stated entirely in terms of those.
//
// file must have been parsed with [go/parser.ParseComments]; without it
// file.Comments is empty and every file scans as unsuppressed.
func scanSuppressions(fset *token.FileSet, file *ast.File) suppressions {
	var found suppressions

	for _, group := range file.Comments {
		for _, comment := range group.List {
			reason, ok := parseDirective(comment.Text)
			if !ok {
				continue
			}

			if found == nil {
				found = make(suppressions)
			}

			found[fset.Position(comment.Pos()).Line] = reason
		}
	}

	return found
}

// parseDirective reports whether text is a //nomutant directive, and returns
// the reason following its colon.
//
// Only `//` line comments are directives. Go tooling conventionally spells
// directives as line comments — `//go:`, `//nolint`, `//lint:ignore` — and a
// block comment can span lines or sit mid-expression, where "the line it is on"
// stops being a meaningful anchor. `/* nomutant */` is therefore an ordinary
// comment and suppresses nothing.
//
// The match is exact after trimming: `nomutant` or `nomutant:<reason>`. A
// comment that merely starts with the word, such as `// nomutants are used
// here`, is prose and must not silently disable mutation.
func parseDirective(text string) (reason string, ok bool) {
	if !strings.HasPrefix(text, "//") {
		return "", false
	}

	body := strings.TrimSpace(strings.TrimPrefix(text, "//"))

	rest, isDirective := strings.CutPrefix(body, directive)
	if !isDirective {
		return "", false
	}

	switch {
	case rest == "":
		return "", true
	case strings.HasPrefix(rest, ":"):
		return strings.TrimSpace(rest[1:]), true
	default:
		return "", false
	}
}

// anchored reports whether node carries a //nomutant directive, and the reason
// given for it.
//
// The anchor is statement-scoped, and deliberately narrow — exactly two lines
// are eligible:
//
//   - leading: the line immediately above the node's *first* line
//     (node.Pos()), with no blank-line gap allowed; and
//   - trailing: the node's *last* line (node.End()), i.e. a comment at the end
//     of the line the node finishes on.
//
// Interior lines are not eligible. For a multi-line condition
//
//	if a &&
//	    b {
//
// the directive belongs above the `if a &&` line; above `b` it does not
// suppress the if statement. This is the documented rule rather than an
// oversight: matching any interior line would let a comment inside one
// statement suppress a neighbouring one, and would make the anchor of a long
// function body effectively the whole function.
//
// The same rule read the other way is worth knowing for compound statements:
// their End() is the closing brace, so a trailing directive on an `if`, `for`
// or `switch` goes on the `}` line, not on the header line.
//
// anchored answers only "is this node's own anchor line suppressed". Cascading
// into a suppressed node's children is the caller's job: [mutateFile] returns
// false from its [go/ast.Inspect] visitor on a hit, which skips the whole
// subtree, so no descendant is ever asked.
//
// Three node kinds are never eligible:
//
//   - [go/ast.File], whose Pos() is the `package` keyword. A directive written
//     above the package clause would otherwise suppress the entire file as a
//     side effect of a comment that reads like a file-level note. Whole-file
//     suppression is not a feature this tool offers, so the safe reading is
//     that such a comment suppresses nothing.
//   - [go/ast.CommentGroup] and [go/ast.Comment], which the walk reaches
//     through nodes' Doc fields. A comment is not mutable code, and a directive
//     matching itself would record a suppression for the directive.
func (s suppressions) anchored(fset *token.FileSet, node ast.Node) (reason string, ok bool) {
	if len(s) == 0 {
		return "", false
	}

	switch node.(type) {
	case *ast.File, *ast.CommentGroup, *ast.Comment:
		return "", false
	}

	if reason, ok := s[fset.Position(node.Pos()).Line-1]; ok {
		return reason, true
	}

	if reason, ok := s[fset.Position(node.End()).Line]; ok {
		return reason, true
	}

	return "", false
}
