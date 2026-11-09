Output "/tmp/waitlist-chaos-results.mp4"

Require ./bin/experiment

Set Shell "bash"
Set FontSize 16
Set Width 1500
Set Height 640
Set Padding 30
Set Theme "Catppuccin Mocha"
Set TypingSpeed 10ms

Env DATABASE_URL "postgresql://chaos:chaos@localhost:5433/waitlist_chaos?sslmode=disable"

Type "./bin/experiment 2>/dev/null"
Enter
Wait@180s
Sleep 1500ms
