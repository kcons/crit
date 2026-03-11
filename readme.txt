Code Review InTelligence

This is a tool intended to make "do I have PRs I need to deal with?" trivial to quickly answer, either manually for more information, or at shell startup in in the prompt for ongoing awareness.

For zsh, add to your .zshrc:

    setopt PROMPT_SUBST
    PS1='$(crit -quick -style prompt) %1~ %# '

Single quotes are important: they prevent the command from being
evaluated at assignment time. PROMPT_SUBST tells zsh to re-evaluate
the substitution each time the prompt is displayed.

For bash, add to your .bashrc:

    PROMPT_COMMAND='PS1="$(crit -quick -style prompt) \w \$ "'

Claude Code status line
-----------------------

Claude Code supports a custom status line script that runs on each turn. To
include crit output there, create ~/.claude/statusline.sh:

    #!/bin/bash
    input=$(cat)
    model=$(echo "$input" | jq -r '.model.display_name')
    current_dir=$(echo "$input" | jq -r '.workspace.current_dir')
    remaining=$(echo "$input" | jq -r '.context_window.remaining_percentage // empty')
    cost=$(echo "$input" | jq -r '.cost.total_cost_usd // 0')
    cost=$(printf "%.2f" "$cost")
    crit_output=$(crit --quick --style prompt 2>/dev/null || echo '')

    status="$model | $current_dir"
    [ -n "$remaining" ] && status="$status | Context: ${remaining}%"
    [ "$cost" != "0.00" ] && status="$status | Cost: \$$cost"
    [ -n "$crit_output" ] && status="$status | $crit_output"

    printf "%s" "$status"

Make it executable:

    chmod +x ~/.claude/statusline.sh

Then point Claude Code at it in ~/.claude/settings.json:

    {
      "statusCommand": "~/.claude/statusline.sh"
    }

The --quick flag is important here: it renders from the cached state file
immediately (no blocking network call) and triggers a background refresh if
the cache is stale, keeping the status line snappy.
