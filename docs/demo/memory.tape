# The demo that makes the pitch: agent memory is a file you can fix.
#
# Reproducible on purpose — a screenshot of a terminal is a claim, a tape that
# regenerates it is a check. Regenerate with:
#   vhs docs/demo/memory.tape          (needs vhs, ttyd, ffmpeg)
#
# It drives the real CLI against a throwaway vault, so if supersession ever
# stops working the GIF stops showing it rather than quietly staying true.
#
# The server runs (hidden) for a reason that is not cosmetic: the last beat
# edits the memory file by hand, and it is the running watcher that reconciles
# an external edit into the index. Without it `recall` answers from the stale
# index and the demo shows the opposite of what it claims — which is exactly
# what the first cut of this tape did.

Output docs/screenshots/memory-demo.gif

Require grimoire

Set Shell bash
Set FontSize 16
Set Width 1180
Set Height 640
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 32ms

Hide
Type "export GRIMOIRE_VAULT=$(mktemp -d) GRIMOIRE_LOCAL_EMBED=off GRIMOIRE_PORT=9931 PS1='$ '" Enter
Type "grimoire serve >/dev/null 2>&1 &" Enter
Type "until curl -sf localhost:9931/api/health >/dev/null; do sleep 1; done" Enter
Type "clear" Enter
Show

Sleep 700ms
Type "# your agent learns something and writes it down" Enter
Sleep 400ms
Type `grimoire remember "Billing Postgres runs on port 5432"` Enter
Sleep 1500ms

Type "# later, it corrects itself" Enter
Sleep 400ms
Type `grimoire remember "Billing Postgres runs on port 6543"` Enter
Sleep 2000ms

Type "# that memory is not a database. it is a file." Enter
Sleep 400ms
Type "cat $GRIMOIRE_VAULT/memory/*.md" Enter
Sleep 3500ms

Type "# and it is still wrong. fix it the way you fix any file." Enter
Sleep 400ms
Type "sed -i 's/6543/6432/' $GRIMOIRE_VAULT/memory/*.md" Enter
Sleep 1800ms

Type "grimoire recall postgres" Enter
Sleep 3500ms
