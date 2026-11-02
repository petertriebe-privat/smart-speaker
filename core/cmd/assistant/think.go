package main

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// think_harder: the backend in a conversation reasons little so it answers in seconds
// (live.delegationEffort). A task that needs careful thought goes from there to a second run
// with high reasoning and all tools (the user's wish, 16.10.2026: "Kann the assistant den Denkaufwand
// bei Bedarf hochsetzen?"). Slow: 30-90 s.
// thinkInstructions: THINK_HARDER_INSTRUCTIONS with the tool hints of TASK_INSTRUCTIONS (also for
// the background tasks, background.go).
func thinkInstructions() string {
	instructions := prompts.Text("THINK_HARDER_INSTRUCTIONS")
	const hintsHeading = "# Working with the tools" // TOOL_HINTS in prompts.h
	if hints := prompts.Text("TASK_INSTRUCTIONS"); strings.Contains(hints, hintsHeading) {
		instructions += hints[strings.Index(hints, hintsHeading):]
	} else {
		log.Printf("[Think] %q not in TASK_INSTRUCTIONS: the run goes without the tool hints", hintsHeading)
	}
	return instructions
}

func thinkHarderTool(a *agent.Agent) tools.Tool {
	return tools.Tool{
		Name: "think_harder",
		Description: "Hands a task that needs careful thought to a slower, more thorough run with all tools: " +
			"comparisons and recommendations, plans, exact calculations, research across several sources, " +
			"longer texts, or whenever the user asks you to think it through („denk gründlich nach“). Takes " +
			"30-90 s: tell the user it takes a moment. Never for what your own tools do: music and speakers " +
			"(play_music, music_control, music_speakers), lights, the TV, the weather; the run does not know " +
			"this conversation. Also, with quick true (a few seconds), for what none of your tools covers: the run has the assistant's " +
			"administration too (people of the household, welcome mails, sorting mail into folders, connecting " +
			"services, Bluetooth, listing or deleting automations, pausing or running tasks, whole websites). " +
			"Give the complete task in German with everything the user said that matters; the result comes " +
			"back as text.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"task": {"type": "string", "description": "the complete task in German"},
                           "quick": {"type": "boolean", "description": "true: no careful thought, only the further tools"}},
            "required": ["task"]}`,
		Channels: tools.Voice | tools.Console, // a conversation (and tests); the run itself does not hand on again
		Run: func(args map[string]any, ctx tools.Context) string {
			task := strings.TrimSpace(tools.String(args, "task"))
			if task == "" {
				return "Error: the task is missing"
			}
			instructions := thinkInstructions()
			effort := "high"
			if quick, _ := args["quick"].(bool); quick {
				effort = "low"
			}
			started := time.Now()
			out := a.Run(task, agent.Options{Instructions: instructions, Person: ctx.Person, Origin: ctx.Origin,
				Effort: effort, Exclude: []string{"think_harder"}})
			log.Printf("[Think] %.80q: %d s", task, int(time.Since(started).Seconds()))
			return out
		},
	}
}
