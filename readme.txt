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
