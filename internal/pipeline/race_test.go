//go:build integration && race

package pipeline

func init() { raceEnabled = true }
