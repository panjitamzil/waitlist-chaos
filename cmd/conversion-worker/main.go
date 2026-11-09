package main

import "waitlist-chaos/internal/worker"

func main() {
	worker.Main("conversion-worker", worker.NewConversion)
}
