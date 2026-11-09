Output "/tmp/waitlist-chaos-s3-safe.mp4"

Require ./bin/experiment

Set Shell "bash"
Set FontSize 14
Set Width 1600
Set Height 440
Set Padding 30
Set Theme "Catppuccin Mocha"
Set TypingSpeed 10ms

Env DATABASE_URL "postgresql://chaos:chaos@localhost:5433/waitlist_chaos?sslmode=disable"

Type "./bin/experiment -scenario s3 -variant safe -v"
Enter
Wait@60s
Sleep 1500ms
