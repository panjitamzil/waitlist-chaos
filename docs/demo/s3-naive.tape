Output "/tmp/waitlist-chaos-s3-naive.mp4"

Require ./bin/experiment
Require ./bin/oracle

Set Shell "bash"
Set FontSize 16
Set Width 1500
Set Height 360
Set Padding 30
Set Theme "Catppuccin Mocha"
Set TypingSpeed 10ms

Env DATABASE_URL "postgresql://chaos:chaos@localhost:5433/waitlist_chaos?sslmode=disable"

Hide
Type "ev=$(./bin/experiment -scenario s3 -variant naive 2>/dev/null | grep -oE '[0-9a-f]{8}-[0-9a-f-]{27}' | head -1); sleep 6; clear"
Enter
Wait@60s
Show

Type "./bin/oracle -event $ev"
Enter
Wait+Screen@30s /L-1/
Sleep 1500ms
