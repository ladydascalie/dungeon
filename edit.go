package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// `dungeon edit` changes one field of one unit, or of one minion, in place.
//
// The plan is edited as text. A yaml round trip drops the blank lines and
// rewraps every folded goal, so the diff would be the whole file; the yaml tree
// is used only to find the lines a field occupies. Nothing is written unless the
// edited plan still parses and adds no finding: an edit that breaks the plan is
// a half-edit the next command would have to finish.

// target names one object in a plan.
type target struct {
	section string // units or minions
	id      string
}

func (t target) String() string {
	return strings.TrimSuffix(t.section, "s") + " " + t.id
}

// repeatable is a flag that may be given more than once.
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, " ") }

func (r *repeatable) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("an empty value names nothing")
	}
	*r = append(*r, v)
	return nil
}

// scalarEdit sets one field. An empty value deletes the field.
type scalarEdit struct {
	key   string
	value string
}

// listEdit applies to one list field in three stages: set replaces the list,
// add appends what is missing, rm removes what must be there. An add that
// changes nothing is not a change; an rm of an absent entry is an error, because
// it is a typo in the command.
type listEdit struct {
	key string
	set repeatable
	add repeatable
	rm  repeatable
}

func (e listEdit) any() bool {
	return e.set != nil || e.add != nil || e.rm != nil
}

func editCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("edit", stderr)
	planPath, _ := planFlags(fs)
	dryRun := fs.Bool("dry-run", false, "print the change and write nothing")

	minion := fs.String("minion", "", "the minion that owns the unit")
	goal := fs.String("goal", "", "what the unit changes")
	accept := fs.String("accept", "", "the command that proves the unit")
	verdict := fs.String("verdict", "", "the report line that proves the unit")

	var pathsAdd, pathsSet, pathsRm repeatable
	var depsAdd, depsSet, depsRm repeatable
	fs.Var(&pathsAdd, "paths-add", "append a path the unit owns (repeatable)")
	fs.Var(&pathsSet, "paths-set", "replace the unit's paths (repeatable)")
	fs.Var(&pathsRm, "paths-rm", "remove a path the unit owns (repeatable)")
	fs.Var(&depsAdd, "deps-add", "append a unit this unit waits on (repeatable)")
	fs.Var(&depsSet, "deps-set", "replace the unit's deps (repeatable)")
	fs.Var(&depsRm, "deps-rm", "remove a dep this unit lists (repeatable)")

	role := fs.String("role", "", "scout, worker, reviewer or integrator")
	tools := fs.String("tools", "", "the minion's tool allowlist")
	of := fs.String("of", "", "the writer this minion reads")
	worktree := fs.String("worktree", "", "the minion's worktree name")

	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) != 2 || (rest[0] != "unit" && rest[0] != "minion") {
		fmt.Fprintln(stderr, "usage: dungeon edit unit|minion <id> [flags]")
		return 2
	}
	t := target{section: rest[0] + "s", id: rest[1]}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var foreign [][2]string
	if rest[0] == "unit" {
		foreign = [][2]string{{"role", "minion"}, {"tools", "minion"}, {"of", "minion"}, {"worktree", "minion"}}
	} else {
		foreign = [][2]string{
			{"minion", "unit"},
			{"goal", "unit"},
			{"accept", "unit"},
			{"verdict", "unit"},
			{"paths-add", "unit"},
			{"paths-set", "unit"},
			{"paths-rm", "unit"},
			{"deps-add", "unit"},
			{"deps-set", "unit"},
			{"deps-rm", "unit"},
		}
	}
	for _, f := range foreign {
		if given[f[0]] {
			fmt.Fprintf(stderr, "dungeon: --%s changes a %s, not a %s\n", f[0], f[1], rest[0])
			return 2
		}
	}

	var scalars []scalarEdit
	var lists []listEdit
	if rest[0] == "unit" {
		for _, s := range []scalarEdit{
			{"minion", *minion}, {"goal", *goal}, {"accept", *accept}, {"verdict", *verdict},
		} {
			if given[s.key] {
				scalars = append(scalars, s)
			}
		}
		lists = []listEdit{
			{key: "paths", set: pathsSet, add: pathsAdd, rm: pathsRm},
			{key: "deps", set: depsSet, add: depsAdd, rm: depsRm},
		}
	} else {
		for _, s := range []scalarEdit{
			{"role", *role}, {"tools", *tools}, {"of", *of}, {"worktree", *worktree},
		} {
			if given[s.key] {
				scalars = append(scalars, s)
			}
		}
		lists = []listEdit{{key: "paths"}, {key: "deps"}}
	}

	if len(scalars) == 0 && !anyList(lists) {
		fmt.Fprintf(stderr, "dungeon: edit %s names no field to change\n", t)
		fmt.Fprintln(stderr, "dungeon: pass at least one flag, for example --paths-add 'api/**'")
		return 2
	}

	if code := resolvePlan(fs, planPath, stderr); code != 0 {
		return code
	}
	src, err := os.ReadFile(*planPath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}

	text := src
	changed := false
	for _, s := range scalars {
		text, changed, err = editScalar(text, t, s)
		if err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
	}
	for _, l := range lists {
		if !l.any() {
			continue
		}
		out, did, err := editList(text, t, l)
		if err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		text, changed = out, changed || did
	}

	if !changed {
		fmt.Fprintf(stdout, "%s: nothing to change\n", t)
		return 0
	}

	_, beforeFindings := validPlanBytes(src)
	after, afterFindings := validPlanBytes(text)
	if added := newFindings(beforeFindings, afterFindings); len(added) > 0 {
		for _, f := range added {
			fmt.Fprintf(stderr, "dungeon: edit refused, it adds %s\n", f)
		}
		fmt.Fprintf(stderr, "dungeon: %s is unchanged\n", *planPath)
		return 1
	}

	fmt.Fprint(stdout, unifiedDiff(*planPath, planLines(src), planLines(text), 3))
	if *dryRun {
		fmt.Fprintln(stdout, "dry run: nothing written")
		return 0
	}
	if err := writeFileAtomic(*planPath, text); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}

	if len(afterFindings) == 0 {
		fmt.Fprintf(stdout, "ok: %s, %d minions, %d units\n", after.Encounter, len(after.Minions), len(after.Units))
	} else {
		for _, f := range afterFindings {
			fmt.Fprintf(stdout, "note: %s\n", f)
		}
	}
	if st, err := LoadState(StatePath(*planPath)); err == nil && st.PlanHash != "" && st.PlanHash != hashPlan(src) {
		fmt.Fprintf(stdout, "note: the run was dispatched from %s, so next and land need --force\n",
			shortHash(st.PlanHash))
	}
	return 0
}

func anyList(lists []listEdit) bool {
	for _, l := range lists {
		if l.any() {
			return true
		}
	}
	return false
}

// validPlanBytes decodes and validates a plan the way every command does, from
// bytes, so an edit is checked before it reaches the disk.
func validPlanBytes(text []byte) (Plan, []Finding) {
	p, findings := Decode(text)
	if len(findings) > 0 {
		return p, findings
	}
	if findings = Validate(p); len(findings) > 0 {
		return p, findings
	}
	_, findings = ExpandLoops(p)
	return p, findings
}

// newFindings is the multiset difference: what the edit introduced.
func newFindings(before, after []Finding) []Finding {
	seen := map[string]int{}
	for _, f := range before {
		seen[f.String()]++
	}
	var out []Finding
	for _, f := range after {
		key := f.String()
		if seen[key] > 0 {
			seen[key]--
			continue
		}
		out = append(out, f)
	}
	return out
}

func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".plan-*.yaml")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// editScalar sets or, with an empty value, deletes one scalar field.
func editScalar(src []byte, t target, e scalarEdit) ([]byte, bool, error) {
	obj, lines, err := locate(src, t)
	if err != nil {
		return nil, false, err
	}
	field, found := mapField(obj, e.key)
	if found && field.value.Kind == yaml.ScalarNode && field.value.Value == e.value {
		return src, false, nil
	}
	if !found && e.value == "" {
		return src, false, nil
	}

	if isFlow(obj) {
		var value *yaml.Node
		if e.value != "" {
			value = scalarNode(e.value)
		}
		out, err := applyFlow(obj, lines, e.key, value)
		if err != nil {
			return nil, false, err
		}
		return joinLines(out), true, nil
	}

	indent := objectIndent(obj)
	if found {
		indent = field.key.Column - 1
	}
	var repl []string
	if e.value != "" {
		repl, err = scalarLines(e.key, e.value, indent)
		if err != nil {
			return nil, false, err
		}
	}
	out, err := applyBlock(lines, obj, e.key, repl)
	if err != nil {
		return nil, false, err
	}
	return joinLines(out), true, nil
}

// editList applies a list change: set, then add, then rm.
func editList(src []byte, t target, e listEdit) ([]byte, bool, error) {
	obj, lines, err := locate(src, t)
	if err != nil {
		return nil, false, err
	}
	field, found := mapField(obj, e.key)
	var current []string
	flow := false
	if found {
		if field.value.Kind != yaml.SequenceNode && !isNull(field.value) {
			return nil, false, fmt.Errorf("%s: %s is not a list", t, e.key)
		}
		current, err = stringSeq(field.value)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %v", t, err)
		}
		flow = field.value.Style&yaml.FlowStyle != 0
	}

	next, missing := buildList(current, e)
	if len(missing) > 0 {
		return nil, false, fmt.Errorf("%s: %s does not list %q", t, e.key, missing[0])
	}
	if slices.Equal(current, next) && (found || len(next) == 0) {
		return src, false, nil
	}

	if isFlow(obj) {
		var value *yaml.Node
		if len(next) > 0 {
			value = seqNode(next)
		}
		out, err := applyFlow(obj, lines, e.key, value)
		if err != nil {
			return nil, false, err
		}
		return joinLines(out), true, nil
	}

	indent := objectIndent(obj)
	if found {
		indent = field.key.Column - 1
	}
	var repl []string
	if len(next) > 0 {
		repl, err = listLines(e.key, next, indent, flow)
		if err != nil {
			return nil, false, err
		}
	}
	out, err := applyBlock(lines, obj, e.key, repl)
	if err != nil {
		return nil, false, err
	}
	return joinLines(out), true, nil
}

// buildList returns the list after the edit, and the first rm entry the list
// never had.
func buildList(current []string, e listEdit) ([]string, []string) {
	out := slices.Clone(current)
	if e.set != nil {
		out = slices.Clone([]string(e.set))
	}
	for _, v := range e.add {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	for _, v := range e.rm {
		if !slices.Contains(out, v) {
			return nil, []string{v}
		}
		for i := len(out) - 1; i >= 0; i-- {
			if out[i] == v {
				out = slices.Delete(out, i, i+1)
			}
		}
	}
	return out, nil
}

type fieldNode struct {
	key   *yaml.Node
	value *yaml.Node
}

// locate finds one unit or minion mapping and the plan's lines.
func locate(src []byte, t target) (*yaml.Node, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, fmt.Errorf("the plan does not parse: %v", err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("the plan is not a mapping")
	}
	seq, ok := mapField(root, t.section)
	if !ok || seq.value.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("the plan has no %s list", t.section)
	}
	for _, item := range seq.value.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if f, ok := mapField(item, "id"); ok && f.value.Value == t.id {
			return item, planLines(src), nil
		}
	}
	return nil, nil, fmt.Errorf("%s is not in the plan", t)
}

func mapField(m *yaml.Node, key string) (fieldNode, bool) {
	if m.Kind != yaml.MappingNode {
		return fieldNode{}, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return fieldNode{key: m.Content[i], value: m.Content[i+1]}, true
		}
	}
	return fieldNode{}, false
}

func isFlow(n *yaml.Node) bool { return n.Style&yaml.FlowStyle != 0 }

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func stringSeq(n *yaml.Node) ([]string, error) {
	if n == nil || isNull(n) {
		return nil, nil
	}
	var out []string
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("a list entry is not a value")
		}
		out = append(out, item.Value)
	}
	return out, nil
}

// planLines splits the plan into lines without a trailing empty element, so a
// line number is the number the operator sees.
func planLines(src []byte) []string {
	lines := strings.Split(string(src), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func indentAt(lines []string, line int) int {
	l := lines[line-1]
	return len(l) - len(strings.TrimLeft(l, " "))
}

// objectIndent is the column of an object's keys.
func objectIndent(obj *yaml.Node) int {
	if len(obj.Content) > 0 {
		return obj.Content[0].Column - 1
	}
	return obj.Column - 1
}

// fieldSpan is the 1-based inclusive line range a key's field occupies: its own
// line plus every following line indented deeper than it. Blank lines inside it
// belong to the field; blank lines after the last content do not.
func fieldSpan(lines []string, key *yaml.Node) (start, end int) {
	start, end = key.Line, key.Line
	indent := indentAt(lines, start)
	for i := start + 1; i <= len(lines); i++ {
		if strings.TrimSpace(lines[i-1]) == "" {
			continue
		}
		if indentAt(lines, i) <= indent {
			break
		}
		end = i
	}
	return start, end
}

// objectEnd is the last line of the object's last field.
func objectEnd(lines []string, obj *yaml.Node) int {
	end := obj.Line
	for i := 0; i+1 < len(obj.Content); i += 2 {
		if _, e := fieldSpan(lines, obj.Content[i]); e > end {
			end = e
		}
	}
	return end
}

// splice replaces lines[start..end] with repl. start = end+1 inserts.
func splice(lines []string, start, end int, repl []string) []string {
	out := make([]string, 0, len(lines)-(end-start+1)+len(repl))
	out = append(out, lines[:start-1]...)
	out = append(out, repl...)
	out = append(out, lines[end:]...)
	return out
}

// applyBlock replaces or deletes one field in a block mapping. An empty repl
// deletes; a missing field with a repl appends after the last field.
func applyBlock(lines []string, obj *yaml.Node, key string, repl []string) ([]string, error) {
	field, found := mapField(obj, key)
	if found {
		start, end := fieldSpan(lines, field.key)
		return splice(lines, start, end, repl), nil
	}
	if len(repl) == 0 {
		return lines, nil
	}
	at := objectEnd(lines, obj)
	return splice(lines, at+1, at, repl), nil
}

// applyFlow rebuilds a one-line flow mapping with one field replaced. A nil
// value deletes the field.
func applyFlow(obj *yaml.Node, lines []string, key string, value *yaml.Node) ([]string, error) {
	if obj.Line < 1 || obj.Line > len(lines) || obj.Column < 2 {
		return nil, fmt.Errorf("cannot place %s in a flow mapping", key)
	}
	for i := 0; i+1 < len(obj.Content); i += 2 {
		if k, v := obj.Content[i], obj.Content[i+1]; k.Line != obj.Line || v.Line != obj.Line {
			return nil, fmt.Errorf("the mapping spans more than one line: add %s by hand", key)
		}
	}
	next := make([]*yaml.Node, 0, len(obj.Content)+2)
	replaced := false
	for i := 0; i+1 < len(obj.Content); i += 2 {
		k, v := obj.Content[i], obj.Content[i+1]
		if k.Value == key {
			replaced = true
			if value != nil {
				next = append(next, k, value)
			}
			continue
		}
		next = append(next, k, v)
	}
	if !replaced && value != nil {
		next = append(next, scalarNode(key), value)
	}
	out, err := marshalNode(&yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle, Content: next})
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "\n") {
		return nil, fmt.Errorf("the mapping no longer fits on one line: add %s by hand", key)
	}
	line := lines[obj.Line-1]
	prefix := line[:obj.Column-1]
	suffix := ""
	if i := strings.Index(line[obj.Column-1:], " #"); i >= 0 {
		suffix = line[obj.Column-1+i:]
	}
	lines[obj.Line-1] = prefix + out + suffix
	return lines, nil
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func seqNode(values []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range values {
		n.Content = append(n.Content, scalarNode(v))
	}
	return n
}

func marshalNode(n *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// scalarText returns the yaml rendering of a value: the text after the colon,
// and the continuation lines of a block scalar, already indented by 2.
func scalarText(v string) (string, []string, error) {
	out, err := marshalNode(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalarNode("v"), scalarNode(v),
	}})
	if err != nil {
		return "", nil, err
	}
	head, rest, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(head, "v: ") {
		return "", nil, fmt.Errorf("cannot write %q as a value", v)
	}
	var extra []string
	if rest != "" {
		extra = strings.Split(rest, "\n")
	}
	return strings.TrimPrefix(head, "v: "), extra, nil
}

func spaces(n int) string { return strings.Repeat(" ", n) }

// scalarLines renders one scalar field. A long single-line value is written as a
// folded block, so a goal stays readable in the file and in a review.
func scalarLines(key, value string, indent int) ([]string, error) {
	if folded, ok := foldedLines(key, value, indent); ok {
		return folded, nil
	}
	head, rest, err := scalarText(value)
	if err != nil {
		return nil, err
	}
	out := []string{spaces(indent) + key + ": " + head}
	for _, l := range rest {
		out = append(out, spaces(indent)+l)
	}
	return out, nil
}

// foldedLines wraps a long one-line value into a folded block. It gives up on
// anything a fold would change: a newline, a repeated space, a leading or
// trailing space.
func foldedLines(key, value string, indent int) ([]string, bool) {
	if len(value) <= 78 || strings.Contains(value, "\n") {
		return nil, false
	}
	if strings.TrimSpace(value) != value || strings.Contains(value, "  ") {
		return nil, false
	}
	words := strings.Split(value, " ")
	if len(words) < 2 {
		return nil, false
	}
	out := []string{spaces(indent) + key + ": >-"}
	line := ""
	for _, w := range words {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) <= 78:
			line += " " + w
		default:
			out = append(out, spaces(indent+2)+line)
			line = w
		}
	}
	if line != "" {
		out = append(out, spaces(indent+2)+line)
	}
	return out, true
}

func listLines(key string, values []string, indent int, flow bool) ([]string, error) {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		head, rest, err := scalarText(v)
		if err != nil {
			return nil, err
		}
		if len(rest) > 0 {
			return nil, fmt.Errorf("cannot write %q as a list entry", v)
		}
		quoted = append(quoted, head)
	}
	if flow {
		return []string{spaces(indent) + key + ": [" + strings.Join(quoted, ", ") + "]"}, nil
	}
	out := []string{spaces(indent) + key + ":"}
	for _, q := range quoted {
		out = append(out, spaces(indent+2)+"- "+q)
	}
	return out, nil
}

type diffOp struct {
	kind byte // ' ', '-' or '+'
	text string
}

func lineDiff(before, after []string) []diffOp {
	n, m := len(before), len(after)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case before[i] == after[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case before[i] == after[j]:
			ops = append(ops, diffOp{' ', before[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', before[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', after[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', before[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', after[j]})
	}
	return ops
}

// unifiedDiff prints only the changed neighbourhood, the way git does.
func unifiedDiff(path string, before, after []string, context int) string {
	ops := lineDiff(before, after)
	var b strings.Builder
	var changed []int
	for i, op := range ops {
		if op.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)

	starts := make([][2]int, len(ops))
	oldNo, newNo := 1, 1
	for i, op := range ops {
		starts[i] = [2]int{oldNo, newNo}
		switch op.kind {
		case ' ':
			oldNo++
			newNo++
		case '-':
			oldNo++
		case '+':
			newNo++
		}
	}

	for i := 0; i < len(changed); {
		first := changed[i]
		last := first
		i++
		for i < len(changed) && changed[i]-last <= 2*context+1 {
			last = changed[i]
			i++
		}
		from, to := first-context, last+context
		if from < 0 {
			from = 0
		}
		if to > len(ops)-1 {
			to = len(ops) - 1
		}
		oldCount, newCount := 0, 0
		for k := from; k <= to; k++ {
			switch ops[k].kind {
			case ' ':
				oldCount++
				newCount++
			case '-':
				oldCount++
			case '+':
				newCount++
			}
		}
		oldStart, newStart := starts[from][0], starts[from][1]
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for k := from; k <= to; k++ {
			b.WriteByte(ops[k].kind)
			b.WriteString(ops[k].text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
