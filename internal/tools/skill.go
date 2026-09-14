package tools

import (
	"fmt"
	"strings"
)

// ReadSkillTool loads the full body of a skill by name (the Claude Code skill
// loading idea: descriptions ride in the system prompt, bodies load on demand).
type ReadSkillTool struct{}

// NewReadSkillTool creates the ReadSkill tool.
func NewReadSkillTool() *ReadSkillTool { return &ReadSkillTool{} }

func (t *ReadSkillTool) Name() string { return "ReadSkill" }

func (t *ReadSkillTool) Description() string {
	return `Load the full instructions of a skill by name. Skills are reusable
capability packs (workflows, style guides, domain playbooks) available in this
session. The system prompt lists their names and one-line descriptions; call
this tool with the exact name when you need the full skill. Returns the skill's
markdown body.`
}

func (t *ReadSkillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The exact skill name to load.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *ReadSkillTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	name := StringArg(ctx.Args, "name", "")
	if name == "" {
		return "", fmt.Errorf("ReadSkill: name is required")
	}
	if ctx.Skills == nil {
		return "", fmt.Errorf("ReadSkill: skills are not available in this session")
	}
	sk, ok := ctx.Skills.Get(name)
	if !ok {
		all := ctx.Skills.All()
		names := make([]string, 0, len(all))
		for _, s := range all {
			names = append(names, s.Name)
		}
		return "", fmt.Errorf("ReadSkill: no skill %q (available: %s)", name, strings.Join(names, ", "))
	}
	return boundedToolString(ctx, fmt.Sprintf("# Skill: %s (source: %s)\n\n%s", sk.Name, sk.Source, sk.Body)), nil
}
