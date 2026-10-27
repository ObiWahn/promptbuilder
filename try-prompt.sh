# Source this to try the promptbuilder prompt in your current shell:
#
#   source /home/claude/remote-repos/02_CONFIG/bash-promot/try-prompt.sh
#
# Defines promptbuilder_activate / promptbuilder_restore so you can toggle
# back and forth as many times as you like in this shell, and activates it
# once right away. Only changes PROMPT_COMMAND/PS1 for this one session -
# your ~/.bashrc.d setup is untouched, and any new shell still gets the old
# prompt.
#
# Do NOT run this file directly (`./try-prompt.sh`) - it has to be sourced
# so the PROMPT_COMMAND/PS1 changes apply to your interactive shell rather
# than a throwaway subshell.

if ! command -v promptbuilder >/dev/null 2>&1; then
    echo "try-prompt.sh: 'promptbuilder' not found on \$PATH - build/install it first (e.g. ~/.bin/promptbuilder)" >&2
    return 1 2>/dev/null || exit 1
fi

promptbuilder_activate() {
    # Only save the true original the first time - if this is called again
    # while already active (or after a restore), the saved values must
    # stay the real original, not promptbuilder's own PROMPT_COMMAND/PS1.
    if [[ -z "${_PROMPTBUILDER_OLD_PROMPT_COMMAND+set}" ]]; then
        _PROMPTBUILDER_OLD_PROMPT_COMMAND="$PROMPT_COMMAND"
        _PROMPTBUILDER_OLD_PS1="$PS1"
    fi

    # Same "capture $? before anything else touches it, then keep
    # incrementally appending history" shape as the current
    # ~/.bashrc.d/all/prompt setup - just building PS1 via promptbuilder
    # instead of __git_ps1.
    #
    # The output goes into a variable that PS1 only *references*: bash
    # expands $(...) and `...` inside PS1 itself on every render, so
    # putting the output into PS1 directly would execute any such text in
    # a directory or branch name. A variable's value is not expanded again.
    PROMPT_COMMAND='_pb_exit=$?; history -a; _pb_ps1=$(promptbuilder "$_pb_exit")'
    PS1='${_pb_ps1}'
    echo "promptbuilder: prompt active for this shell. Run 'promptbuilder_restore' to switch back."
}

promptbuilder_restore() {
    if [[ -z "${_PROMPTBUILDER_OLD_PROMPT_COMMAND+set}" ]]; then
        echo "promptbuilder: nothing to restore - never activated in this shell" >&2
        return 1
    fi
    PROMPT_COMMAND="$_PROMPTBUILDER_OLD_PROMPT_COMMAND"
    PS1="$_PROMPTBUILDER_OLD_PS1"
    echo "promptbuilder: restored your previous prompt. Run 'promptbuilder_activate' to switch back."
}

promptbuilder_activate
