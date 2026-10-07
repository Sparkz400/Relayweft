package orchestrator

import "github.com/sparkz400/relayweft/internal/config"

// classic turns off the task-shape shortcuts (shape.go), so a test task is
// planned and reviewed with the full planner and reviewer routes, as before
// they existed. Tests of the shortcuts turn them on again.
func classic(c *config.Config) { c.Orchestrator.Classic() }
