# Security

firetower reads every process's command line in order to keep it off the screen, and its output is
made to be pasted into chats and screenshots, so a leak here matters. Please report vulnerabilities
privately through GitHub's
[security advisories](https://github.com/multiplyco/firetower/security/advisories/new), not in a
public issue.

What it is designed to guarantee:

- Command lines are read but never printed. A row shows at most a one-word hint that passes a check
  for names (a dotted name, or a short lowercase subcommand); anything that looks like a key is left
  out.
- Program names come from the kernel, never from `argv[0]`, which a process can set to anything.
- It only reads: it writes no files and changes nothing. From Claude Code it reads the session files
  and takes only the titles from each transcript.
- It makes no network connections.

A report that breaks one of these, above all a secret that reaches the screen, is exactly what we
want to hear about.
