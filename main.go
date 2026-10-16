// Command firetower shows what is burning your CPU right now, and who started
// it. macOS only.
//
// One screen: load against cores, the user/sys/idle split, memory and power,
// the busiest processes with where they run and who launched them, and CPU
// totals per program. Command lines are read but never printed, since they
// carry tokens and passwords; only a vetted one-word hint is shown.
package main

/*
#include <libproc.h>
#include <sys/ioctl.h>
#include <unistd.h>

static int term_cols(int fd) {
	struct winsize ws;
	return ioctl(fd, TIOCGWINSZ, &ws) == 0 ? ws.ws_col : 0;
}
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

const version = "0.1.0"

const usage = `usage: firetower [-hV] [-n rows] [-s seconds] [rows]

  -n rows     how many of the busiest processes to list (default 12);
              a bare number does the same: firetower 5
  -s seconds  sampling window in whole seconds (default 2)
  -h          show this help
  -V          show the version

Set NO_COLOR to turn color off. See firetower(1) for how to read the output.
`

// agents are the process names counted as coding agents. Claude Code
// sessions are also found through its registry, whatever their name.
var agents = set("aider", "amp", "claude", "codex", "cursor-agent", "gemini", "goose", "opencode")

// interpreters are programs whose first argument says what they run: a
// script or a subcommand.
var interpreters = set("bash", "bb", "bun", "bunx", "deno", "fish", "node", "npx", "perl", "ruby", "sh", "uv", "zsh")

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Exiting: cleanups (the spinner, the top child) run on every exit path,
// including signals.

var (
	cleanupMu sync.Mutex
	cleanups  []func()
)

func atExit(f func()) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanups = append(cleanups, f)
}

func exit(code int) {
	cleanupMu.Lock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	os.Exit(code)
}

func fail(code int, msg string) {
	fmt.Fprintf(os.Stderr, "firetower: %s\n", msg)
	if code == 2 {
		fmt.Fprintln(os.Stderr, strings.SplitN(usage, "\n", 2)[0])
	}
	exit(code)
}

// Helpers

// The system tools firetower runs, by absolute path: a directory early on PATH
// cannot stand in for them.
const (
	psBin     = "/bin/ps"
	topBin    = "/usr/bin/top"
	lsofBin   = "/usr/sbin/lsof"
	pmsetBin  = "/usr/bin/pmset"
	sysctlBin = "/usr/sbin/sysctl"
)

// run returns a command's stdout whatever its exit status; "" if it cannot
// start.
func run(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return string(out)
}

func isTTY(fd int) bool { return C.isatty(C.int(fd)) == 1 }

// terminalWidth is COLUMNS if set, else the terminal's own width.
func terminalWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return int(C.term_cols(1))
}

// exePath is the executable the kernel ran for pid. Unlike argv[0], a process
// cannot rewrite it, and it needs no privileges, even for root's processes.
func exePath(pid int) string {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

// clean replaces control characters, so nothing read from the system can
// drive the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// clipEnd cuts s to w characters, marking the cut at the end.
func clipEnd(s string, w int) string {
	r := []rune(s)
	switch {
	case len(r) <= w:
		return s
	case w < 4:
		return string(r[:max(w, 0)])
	default:
		return string(r[:w-3]) + "..."
	}
}

// clipStart cuts s to w characters, keeping its end; for paths.
func clipStart(s string, w int) string {
	r := []rune(s)
	switch {
	case len(r) <= w:
		return s
	case w < 4:
		return string(r[len(r)-max(w, 0):])
	default:
		return "..." + string(r[len(r)-(w-3):])
	}
}

func pad(s string, w int) string { return fmt.Sprintf("%-*s", w, s) }

// number reads a decimal from top, which may write a comma for the point and
// follow it with a comma or a percent sign.
func number(s string) (float64, bool) {
	s = strings.TrimSuffix(strings.TrimSuffix(s, ","), "%")
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	return f, err == nil
}

// gib is the GiB in a top size such as 512M, 12G or 1.5T.
func gib(s string) float64 {
	if s == "" {
		return 0
	}
	n, ok := number(s[:len(s)-1])
	if !ok {
		return 0
	}
	switch s[len(s)-1] {
	case 'K':
		return n / 1048576
	case 'M':
		return n / 1024
	case 'T':
		return n * 1024
	}
	return n
}

// seconds reads a ps etime, [[dd-]hh:]mm:ss; -1 when unreadable.
func seconds(etime string) int {
	days := 0
	if d, rest, ok := strings.Cut(etime, "-"); ok {
		days, _ = strconv.Atoi(d)
		etime = rest
	}
	total := 0
	for _, part := range strings.Split(etime, ":") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return -1
		}
		total = total*60 + n
	}
	return days*86400 + total
}

// age is a duration in its two largest units: 48d3h, 14h54m, 3m38s, 9s.
func age(s int) string {
	switch {
	case s < 0:
		return ""
	case s >= 86400:
		return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
	case s >= 3600:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}

// Reading the system

type proc struct {
	ppid int
	age  int
	name string
}

type power struct {
	battery bool
	detail  string
}

type system struct {
	load              [3]float64
	usr, sys, idle    float64
	wired, compressed float64
	cpu               map[int]float64
	topName           map[int]string
	topPID            int
	procs             map[int]proc
	claude            map[int]bool
	claudeNames       map[string]bool
	power             *power
	speedLimit        int
	ncpu              int
	mem               float64
	free              int
}

// parseTop reads the second frame of `top -l 2` (the first has no CPU
// deltas): load, the CPU split, memory, and CPU per process.
func parseTop(out string, s *system) bool {
	frames := strings.Split(out, "Processes:")
	frame := frames[len(frames)-1]
	var gotLoad, gotCPU, inTable bool
	for _, line := range strings.Split(frame, "\n") {
		f := strings.Fields(line)
		switch {
		case inTable && len(f) >= 3:
			pid, err := strconv.Atoi(f[0])
			pct, ok := number(f[1])
			if err == nil && ok {
				s.cpu[pid] = pct
				s.topName[pid] = strings.Join(f[2:], " ")
			}
		case strings.HasPrefix(line, "Load Avg:") && len(f) >= 5:
			gotLoad = true
			for i := range s.load {
				var ok bool
				s.load[i], ok = number(f[2+i])
				gotLoad = gotLoad && ok
			}
		case strings.HasPrefix(line, "CPU usage:") && len(f) >= 7:
			var ok1, ok2, ok3 bool
			s.usr, ok1 = number(f[2])
			s.sys, ok2 = number(f[4])
			s.idle, ok3 = number(f[6])
			gotCPU = ok1 && ok2 && ok3
		case strings.HasPrefix(line, "PhysMem:"):
			for i := 1; i < len(f); i++ {
				if strings.HasPrefix(f[i], "wired") {
					s.wired = gib(strings.TrimPrefix(f[i-1], "("))
				}
				if strings.HasPrefix(f[i], "compressor") {
					s.compressed = gib(f[i-1])
				}
			}
		case strings.HasPrefix(line, "PID"):
			inTable = true
		}
	}
	return gotLoad && gotCPU
}

// programName is the kernel's name for a process. ps's ucomm stops at 16
// characters, so a name that long is completed from the executable's path,
// which the kernel also owns. argv[0] is never used: a process can set it to
// anything, secrets included.
func programName(pid int, ucomm string) string {
	if len(ucomm) < 16 {
		return ucomm
	}
	if full := filepath.Base(exePath(pid)); strings.HasPrefix(full, ucomm) {
		return full
	}
	return ucomm
}

func parsePS(out string) map[int]proc {
	procs := map[int]proc{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs[pid] = proc{ppid: ppid, age: seconds(f[2]), name: programName(pid, strings.Join(f[3:], " "))}
	}
	return procs
}

// parsePower reads `pmset -g batt`; nil on a Mac without a battery.
func parsePower(out string) *power {
	for _, line := range strings.Split(out, "\n") {
		if _, detail, ok := strings.Cut(line, "\t"); ok && strings.Contains(line, "InternalBattery") {
			detail, _, _ = strings.Cut(detail, " present")
			return &power{
				battery: strings.Contains(out, "'Battery Power'"),
				detail:  strings.ReplaceAll(detail, ";", ","),
			}
		}
	}
	return nil
}

func parseSpeedLimit(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "CPU_Speed_Limit" {
			n, _ := strconv.Atoi(f[2])
			return n
		}
	}
	return 0
}

// sample reads everything up front. top samples for secs seconds while the
// cheap facts are read alongside; the process table is read after, so it has
// the processes top saw.
func sample(secs int) *system {
	s := &system{cpu: map[int]float64{}, topName: map[int]string{}, free: -1}
	var stdout bytes.Buffer
	top := exec.Command(topBin, "-l", "2", "-s", strconv.Itoa(secs), "-n", "9999", "-o", "cpu",
		"-stats", "pid,cpu,command")
	top.Stdout = &stdout
	if err := top.Start(); err != nil {
		fail(1, "cannot run top")
	}
	s.topPID = top.Process.Pid
	atExit(func() { _ = top.Process.Kill() })

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); s.power = parsePower(run(pmsetBin, "-g", "batt")) }()
	go func() { defer wg.Done(); s.speedLimit = parseSpeedLimit(run(pmsetBin, "-g", "therm")) }()
	go func() {
		defer wg.Done()
		facts := strings.Fields(run(sysctlBin, "-n", "hw.ncpu", "hw.memsize", "kern.memorystatus_level"))
		if len(facts) > 0 {
			s.ncpu, _ = strconv.Atoi(facts[0])
		}
		if len(facts) > 1 {
			memsize, _ := strconv.ParseFloat(facts[1], 64)
			s.mem = memsize / (1 << 30)
		}
		if len(facts) > 2 {
			s.free, _ = strconv.Atoi(facts[2])
		}
	}()

	if err := top.Wait(); err != nil {
		fail(1, "top failed")
	}
	if !parseTop(stdout.String(), s) || s.ncpu == 0 {
		fail(1, "could not read top's output")
	}
	s.procs = parsePS(run(psBin, "-Ao", "pid=,ppid=,etime=,ucomm="))
	wg.Wait()
	return s
}

func pidList(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// argvOf maps pids to command lines. Read only to find a hint, never printed.
func argvOf(pids []int) map[int]string {
	argv := map[int]string{}
	for _, line := range strings.Split(run(psBin, "-ww", "-o", "pid=,args=", "-p", pidList(pids)), "\n") {
		pidStr, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if pid, err := strconv.Atoi(pidStr); ok && err == nil {
			argv[pid] = strings.TrimSpace(args)
		}
	}
	return argv
}

// cwdOf maps pids to working directories, for the processes lsof may inspect.
// -b keeps lsof from blocking on a dead network mount.
func cwdOf(pids []int) map[int]string {
	cwd := map[int]string{}
	pid := 0
	for _, line := range strings.Split(run(lsofBin, "-a", "-b", "-w", "-d", "cwd", "-Fn", "-p", pidList(pids)), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n"):
			cwd[pid] = line[1:]
		}
	}
	return cwd
}

// Claude Code sessions. The registry and transcripts are internal formats, so
// every read is optional: on any surprise a session shows as name and PID.

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// registered returns the PIDs in Claude Code's session registry. A session is
// known by this, not by its process name, which can be a version number.
func registered(dir string) map[int]bool {
	pids := map[int]bool{}
	entries, _ := os.ReadDir(filepath.Join(dir, "sessions"))
	for _, e := range entries {
		if pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json")); err == nil && strings.HasSuffix(e.Name(), ".json") {
			pids[pid] = true
		}
	}
	return pids
}

// tailLines returns the whole lines in the last n bytes of a file.
func tailLines(path string, n int64) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := max(info.Size()-n, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	if start > 0 {
		lines = lines[1:]
	}
	return lines
}

func isSessionID(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef-") == ""
}

// sessionTitle is the newest title in a session's transcript, preferring one
// the user set. Claude Code re-appends titles as a session goes, so the last
// megabyte holds the current one.
func sessionTitle(dir, sessionID string) string {
	var custom, ai string
	projects, _ := os.ReadDir(filepath.Join(dir, "projects"))
	for _, p := range projects {
		for _, line := range tailLines(filepath.Join(dir, "projects", p.Name(), sessionID+".jsonl"), 1<<20) {
			if !strings.Contains(line, `Title"`) {
				continue
			}
			var rec struct {
				CustomTitle string `json:"customTitle"`
				AITitle     string `json:"aiTitle"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil {
				if rec.CustomTitle != "" {
					custom = rec.CustomTitle
				}
				if rec.AITitle != "" {
					ai = rec.AITitle
				}
			}
		}
	}
	if custom != "" {
		return custom
	}
	return ai
}

type session struct {
	status string
	label  string // the title in quotes, else the session's name
}

func (s *session) String() string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.status + " " + s.label)
}

func readSession(dir string, pid int) *session {
	data, err := os.ReadFile(filepath.Join(dir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return nil
	}
	var reg struct {
		SessionID string `json:"sessionId"`
		Status    string `json:"status"`
		Name      string `json:"name"`
	}
	if json.Unmarshal(data, &reg) != nil {
		return nil
	}
	s := &session{status: reg.Status, label: reg.Name}
	if isSessionID(reg.SessionID) {
		if title := sessionTitle(dir, reg.SessionID); title != "" {
			s.label = `"` + title + `"`
		}
	}
	return s
}

// What each process is

func nameChar(r rune) bool {
	return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.:@+-", r))
}

// dotted reports whether s looks like a file, class or module name: a dot, a
// letter, plain name characters, at most 40 of them, and no
// user:password@host shape.
func dotted(s string) bool {
	if s == "" || len(s) > 40 || strings.IndexFunc(s, func(r rune) bool { return !nameChar(r) }) >= 0 {
		return false
	}
	if i := strings.Index(s, ":"); i >= 0 && strings.Contains(s[i:], "@") {
		return false
	}
	return strings.Contains(s, ".") && strings.IndexFunc(s, unicode.IsLetter) >= 0
}

// word reports whether s looks like a name rather than a secret: a dotted
// name, or a short lowercase subcommand such as test or dev:ram with at most
// two digits. Mixed case, many digits and long runs look like keys.
func word(s string) bool {
	if dotted(s) {
		return true
	}
	if s == "" || len(s) > 20 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r < 'a' || r > 'z') && !strings.ContainsRune("_:-", r):
			return false
		}
	}
	return digits <= 2
}

func keep(s string, ok func(string) bool) string {
	if ok(s) {
		return s
	}
	return ""
}

var jvmOptionsWithValues = set("-cp", "-classpath", "--class-path", "-p", "--module-path", "--add-modules",
	"--add-opens", "--add-exports")

// javaHint is the main class, the jar, or the namespace after clojure.main -m.
func javaHint(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case jvmOptionsWithValues[a]:
			i++
		case a == "-jar" && i+1 < len(args):
			return keep(filepath.Base(args[i+1]), word)
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "@"):
		case a == "clojure.main":
			for j := i + 1; j+1 < len(args); j++ {
				if args[j] == "-m" {
					return keep(args[j+1], dotted)
				}
			}
			return a
		default:
			return keep(a, dotted)
		}
	}
	return ""
}

// inlineCode reports whether an option introduces code on the command line
// (bash -lc, node -e, python -c, perl -p): what follows is code, not a name.
func inlineCode(a string) bool {
	switch a {
	case "-p", "--eval", "--print", "--command":
		return true
	}
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, r := range a[1:] {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return strings.ContainsAny(a[1:], "ceE")
}

// scriptHint is an interpreter's module (-m) or script file, or a CLI's
// subcommand. After an option, only a dotted name is taken, since the option
// may have taken the next word as its value (a password, say).
func scriptHint(args []string) string {
	afterOption := false
	for i, a := range args {
		switch {
		case inlineCode(a):
			return ""
		case a == "-m":
			if i+1 < len(args) {
				return keep(args[i+1], dotted)
			}
			return ""
		case strings.HasPrefix(a, "-"):
			afterOption = true
		case afterOption:
			if name := filepath.Base(a); dotted(name) {
				return name
			}
		default:
			return keep(filepath.Base(a), word)
		}
	}
	return ""
}

// hint is what a process runs, from its command line; "" when nothing safe is
// found.
func hint(prog, argv string) string {
	f := strings.Fields(argv)
	if len(f) < 2 {
		return ""
	}
	switch {
	case prog == "java":
		return javaHint(f[1:])
	case interpreters[prog], strings.HasPrefix(strings.ToLower(prog), "python"):
		return scriptHint(f[1:])
	}
	return ""
}

func homeRelative(path string) string {
	home, _ := os.UserHomeDir()
	switch {
	case path == "" || path == "/":
		return ""
	case path == home:
		return "~"
	case strings.HasPrefix(path, home+"/"):
		return "~" + path[len(home):]
	}
	return path
}

// agentAbove is the nearest coding agent above pid: a registered Claude Code
// session or a process named as an agent. The hop limit guards against a
// cycle in a process table that changed while it was read.
func agentAbove(s *system, pid int) int {
	p := s.procs[pid].ppid
	for hops := 0; p > 1 && hops < 30; hops++ {
		if s.claude[p] || s.claudeNames[s.procs[p].name] || agents[s.procs[p].name] {
			return p
		}
		p = s.procs[p].ppid
	}
	return 0
}

// busiest returns the n busiest PIDs, busiest first, leaving out our top.
func busiest(s *system, n int) []int {
	var pids []int
	for pid := range s.cpu {
		if pid != s.topPID {
			pids = append(pids, pid)
		}
	}
	sort.Slice(pids, func(i, j int) bool {
		if s.cpu[pids[i]] != s.cpu[pids[j]] {
			return s.cpu[pids[i]] > s.cpu[pids[j]]
		}
		return pids[i] < pids[j]
	})
	return pids[:min(n, len(pids))]
}

// progName is the name to show: claude for a registered session or a process
// running the same program as one (Claude Code can run as a version number),
// else the kernel's name, else top's.
func progName(s *system, pid int) string {
	switch {
	case s.claude[pid] || s.claudeNames[s.procs[pid].name]:
		return "claude"
	case s.procs[pid].name != "":
		return s.procs[pid].name
	case s.topName[pid] != "":
		return s.topName[pid]
	}
	return "?"
}

type row struct {
	pid, age          int
	cpu               float64
	prog, where, what string
	by, leftover      string
}

// describe builds the table rows for the hot PIDs: what each runs, where, and
// who launched it, up to the coding agent session above it.
func describe(s *system, hot []int) []row {
	agentOf := map[int]int{}
	var pids []int
	seen := map[int]bool{}
	add := func(p int) {
		if p > 0 && !seen[p] {
			seen[p] = true
			pids = append(pids, p)
		}
	}
	for _, pid := range hot {
		add(pid)
		add(s.procs[pid].ppid)
		if a := agentAbove(s, pid); a != 0 {
			agentOf[pid] = a
			add(a)
		}
	}

	var argv, cwd map[int]string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); argv = argvOf(pids) }()
	go func() { defer wg.Done(); cwd = cwdOf(pids) }()
	dir := claudeDir()
	sessions := map[int]*session{}
	for _, p := range pids {
		if s.claude[p] {
			sessions[p] = readSession(dir, p)
		}
	}
	wg.Wait()

	rows := make([]row, 0, len(hot))
	for _, pid := range hot {
		prog := progName(s, pid)
		r := row{pid: pid, age: -1, cpu: s.cpu[pid], prog: prog, where: homeRelative(cwd[pid])}
		if p, ok := s.procs[pid]; ok {
			r.age = p.age
		}
		if agents[prog] {
			r.what = sessions[pid].String()
		} else {
			r.what = hint(prog, argv[pid])
		}

		var by []string
		agent, ppid := agentOf[pid], s.procs[pid].ppid
		if ppid > 1 && ppid != agent {
			pname := progName(s, ppid)
			by = append(by, strings.TrimSpace(pname+" "+hint(pname, argv[ppid])))
		}
		if agent != 0 {
			parts := []string{progName(s, agent), strconv.Itoa(agent)}
			if t := sessions[agent].String(); t != "" {
				parts = append(parts, t)
			}
			if cwd[agent] != "" && cwd[agent] != cwd[pid] {
				parts = append(parts, "@ "+homeRelative(cwd[agent]))
			}
			by = append(by, strings.Join(parts, " "))
			if sess := sessions[agent]; sess != nil && sess.status == "idle" && r.age >= 3600 && r.cpu >= 25 {
				r.leftover = fmt.Sprintf("%s %d has run %s under idle %s session %d; still needed?",
					prog, pid, age(r.age), progName(s, agent), agent)
			}
		}
		r.by = strings.Join(by, " <- ")
		rows = append(rows, r)
	}
	return rows
}

// Printing

type palette struct{ bold, dim, red, yellow, reset string }

func colors(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{"\033[1m", "\033[2m", "\033[31m", "\033[33m", "\033[0m"}
}

func kernelHeavy(s *system) bool { return s.sys > s.usr && s.sys >= 25 }

// notes are plain-language readings of the numbers. Under 10% idle is
// saturated; sys above user (and at least 25%) means the kernel is doing the
// work; under 20% free memory, by the kernel's own measure, is pressure.
func notes(s *system, rows []row) []string {
	var out []string
	if s.idle < 10 {
		out = append(out, "saturated: less than 10% idle across all cores")
	}
	if kernelHeavy(s) {
		out = append(out, "sys > user: the kernel is doing the work (disk flushes, disk images, file events, memory compression)")
	}
	if s.free >= 0 && s.free < 20 {
		out = append(out, fmt.Sprintf("memory pressure: %d%% free, %.0fG compressed", s.free, s.compressed))
	}
	if s.speedLimit > 0 && s.speedLimit < 100 {
		out = append(out, fmt.Sprintf("thermal: CPU speed limited to %d%%", s.speedLimit))
	}
	for _, r := range rows {
		if r.leftover != "" {
			out = append(out, r.leftover)
		}
	}
	return out
}

// header shows load, the CPU split, memory and power. Below 120 columns,
// memory and power get lines of their own.
func header(s *system, cols int, c palette) string {
	ratio := s.load[0] / float64(s.ncpu)
	ratioColor := c.dim
	if ratio >= 1 {
		ratioColor = c.red
	}
	sysColor, idleColor := "", ""
	if kernelHeavy(s) {
		sysColor = c.red
	}
	if s.idle < 10 {
		idleColor = c.red
	}
	gap := "    "
	if cols > 0 && cols < 120 {
		gap = "\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sfiretower%s  %s  %d cores  load %.1f %.1f %.1f  %s(%.1fx cores)%s\n",
		c.bold, c.reset, time.Now().Format("15:04:05"), s.ncpu, s.load[0], s.load[1], s.load[2], ratioColor, ratio, c.reset)
	fmt.Fprintf(&b, "cpu  %.0f%% user  %s%.0f%% sys%s  %s%.0f%% idle%s",
		s.usr, sysColor, s.sys, c.reset, idleColor, s.idle, c.reset)
	fmt.Fprintf(&b, "%smem  %.0fG: %.0fG wired, %.0fG compressed, %d%% free", gap, s.mem, s.wired, s.compressed, max(s.free, 0))
	if s.power != nil {
		source := "AC"
		if s.power.battery {
			source = c.yellow + "BATTERY" + c.reset
		}
		fmt.Fprintf(&b, "%s%s %s", gap, source, s.power.detail)
	}
	b.WriteString("\n")
	return b.String()
}

// leadWidth is %CPU, PID and AGE with the gaps after each: 6+1+6+1+7+2.
const leadWidth = 23

func runeLen(s string) int { return len([]rune(s)) }

// widths sizes PROGRAM, WHERE and WHAT to their longest cells, up to a cap.
// When a line would overflow the terminal, WHERE and WHAT shrink (to about 30%
// and 25% of the room, never below 16 and 12) so LAUNCHED BY stays readable.
func widths(rows []row, cols int) (prog, where, what int) {
	prog, where, what = 7, 5, 4
	by := 0
	for _, r := range rows {
		prog = max(prog, runeLen(r.prog))
		where = max(where, runeLen(r.where))
		what = max(what, runeLen(r.what))
		by = max(by, runeLen(r.by))
	}
	prog, where, what = min(prog, 24), min(where, 48), min(what, 40)
	if room := cols - leadWidth - prog - 2 - 4; cols > 0 && where+what+by > room {
		where = min(where, max(16, room*3/10))
		what = min(what, max(12, room/4))
	}
	return prog, where, what
}

// table lists the hot processes. Below 120 columns each row's context gets a
// line of its own; wider, it sits in aligned columns.
func table(rows []row, cols int, c palette) string {
	progW, whereW, whatW := widths(rows, cols)
	stacked := cols > 0 && cols < 120
	fit := func(s string) string {
		if cols > 0 {
			return clipEnd(s, cols)
		}
		return s
	}
	heat := func(pct float64) string {
		switch {
		case pct >= 100:
			return c.red
		case pct >= 50:
			return c.yellow
		}
		return ""
	}
	var b strings.Builder
	b.WriteString(c.dim)
	if stacked {
		fmt.Fprintf(&b, "%6s %6s %7s  %s\n%8sWHERE  WHAT  LAUNCHED BY", "%CPU", "PID", "AGE", "PROGRAM", "")
	} else {
		b.WriteString(fit(fmt.Sprintf("%6s %6s %7s  %s  %s  %s  LAUNCHED BY", "%CPU", "PID", "AGE",
			pad("PROGRAM", progW), pad("WHERE", whereW), pad("WHAT", whatW))))
	}
	b.WriteString(c.reset + "\n")
	for _, r := range rows {
		lead := fmt.Sprintf("%5.0f%% %6d %7s  %s", r.cpu, r.pid, age(r.age), pad(clipEnd(clean(r.prog), progW), progW))
		var line string
		if stacked {
			line = strings.TrimRight(lead, " ")
		} else {
			line = fit(strings.TrimRight(lead+"  "+pad(clipStart(clean(r.where), whereW), whereW)+"  "+
				pad(clipEnd(clean(r.what), whatW), whatW)+"  "+clean(r.by), " "))
		}
		b.WriteString(heat(r.cpu) + line[:6] + c.reset + line[6:] + "\n")
		if stacked {
			var context []string
			for _, part := range []string{r.where, r.what, r.by} {
				if part != "" {
					context = append(context, clean(part))
				}
			}
			if len(context) > 0 {
				b.WriteString("        " + c.dim + clipEnd(strings.Join(context, "  "), cols-8) + c.reset + "\n")
			}
		}
	}
	return b.String()
}

// totals adds up CPU per program over every process sampled: the eight
// largest, then the coding agents' share if it did not make that list.
func totals(s *system, cols int, c palette) string {
	type group struct {
		name  string
		n     int
		total float64
	}
	byName := map[string]*group{}
	for pid, pct := range s.cpu {
		if pid == s.topPID {
			continue
		}
		name := progName(s, pid)
		if byName[name] == nil {
			byName[name] = &group{name: name}
		}
		byName[name].n++
		byName[name].total += pct
	}
	groups := make([]*group, 0, len(byName))
	for _, g := range byName {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].total != groups[j].total {
			return groups[i].total > groups[j].total
		}
		return groups[i].name < groups[j].name
	})
	item := func(g *group) string {
		if g.n > 1 {
			return fmt.Sprintf("%s x%d %.0f%%", clean(g.name), g.n, g.total)
		}
		return fmt.Sprintf("%s %.0f%%", clean(g.name), g.total)
	}
	wrap := func(items []string) string {
		if cols <= 0 {
			return strings.Join(items, "   ")
		}
		var lines []string
		line := ""
		for _, it := range items {
			switch {
			case line == "":
				line = it
			case 11+runeLen(line)+3+runeLen(it) > cols:
				lines = append(lines, line)
				line = it
			default:
				line += "   " + it
			}
		}
		return strings.Join(append(lines, line), "\n"+strings.Repeat(" ", 11))
	}
	var shown, missing []string
	listed := map[string]bool{}
	for i, g := range groups {
		if i >= 8 || g.total < 0.5 {
			break
		}
		shown = append(shown, item(g))
		listed[g.name] = true
	}
	for _, g := range groups {
		if agents[g.name] && !listed[g.name] {
			missing = append(missing, item(g))
		}
	}
	out := "\n" + c.dim + "by program" + c.reset + " " + wrap(shown) + "\n"
	if len(missing) > 0 {
		out += c.dim + "agents" + c.reset + "     " + wrap(missing) + "\n"
	}
	return out
}

// Main

// startSpinner draws a spinner on stderr until the returned function is
// called, which clears it. Nothing is drawn when stderr is not a terminal.
func startSpinner(text string) func() {
	if !isTTY(2) {
		return func() {}
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			fmt.Fprintf(os.Stderr, "\r%c %s", frames[i%len(frames)], text)
			select {
			case <-done:
				fmt.Fprint(os.Stderr, "\r\033[K")
				return
			case <-tick.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); <-finished }) }
}

// parseArgs reads the command line BSD style: -n 5, -n5, a bare 5, and
// clustered flags such as -hV. It exits on help, version or misuse.
func parseArgs(args []string) (rows, secs int) {
	rows, secs = 12, 2
	positive := func(v, what string) int {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail(2, what+" must be a positive whole number")
		}
		return n
	}
	gotN, gotRows := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help":
			fmt.Print(usage)
			exit(0)
		case a == "--version":
			fmt.Println("firetower", version)
			exit(0)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				switch flag := a[j]; flag {
				case 'h':
					fmt.Print(usage)
					exit(0)
				case 'V':
					fmt.Println("firetower", version)
					exit(0)
				case 'n', 's':
					value := a[j+1:]
					if value == "" {
						if i+1 >= len(args) {
							fail(2, "-"+string(flag)+" needs a value")
						}
						i++
						value = args[i]
					}
					if flag == 'n' {
						rows, gotN = positive(value, "rows"), true
					} else {
						secs = positive(value, "-s")
					}
					j = len(a)
				default:
					fail(2, "unknown option: -"+string(flag))
				}
			}
		case !gotRows:
			rows, gotRows = positive(a, "rows"), true
		default:
			fail(2, "unexpected argument: "+a)
		}
	}
	if gotN && gotRows {
		fail(2, "give the row count once, with -n or as a number")
	}
	return rows, secs
}

func main() {
	rows, secs := parseArgs(os.Args[1:])

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-signals
		exit(128 + int(sig.(syscall.Signal)))
	}()

	stop := startSpinner(fmt.Sprintf("sampling for %ds", secs))
	atExit(stop)
	s := sample(secs)
	s.claude = registered(claudeDir())
	s.claudeNames = map[string]bool{}
	for pid := range s.claude {
		if name := s.procs[pid].name; name != "" {
			s.claudeNames[name] = true
		}
	}
	hotRows := describe(s, busiest(s, rows))

	outTTY := isTTY(1)
	cols := 0
	if outTTY {
		cols = terminalWidth()
	}
	c := colors(outTTY && os.Getenv("NO_COLOR") == "")
	var b strings.Builder
	b.WriteString(header(s, cols, c))
	for _, n := range notes(s, hotRows) {
		b.WriteString(c.yellow + "->" + c.reset + " " + n + "\n")
	}
	b.WriteString("\n" + table(hotRows, cols, c) + totals(s, cols, c))

	stop()
	fmt.Print(b.String())
}
