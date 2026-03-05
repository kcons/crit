Code Review InTelligence

This is a tool intended to make "do I have PRs I need to deal with?" trivial to quickly answer, either manually for more information, or at shell startup in in the prompt for ongoing awareness.

Do `export PS1="$(crit -quick -style prompt) %1~ %# "`
