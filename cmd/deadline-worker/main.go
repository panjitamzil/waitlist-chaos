package main

import "waitlist-chaos/internal/worker"

func main() {
	worker.Main("deadline-worker", worker.NewDeadline)
}
