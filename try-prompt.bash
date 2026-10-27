# Source this to try the promptbuilder prompt in your current shell:
#
#   source /path/to/bash-prompt/try-prompt.bash
#
# Uses this repo's own build (`make build`), called by its absolute path -
# never looked up in $PATH. To try another binary instead, call
# `promptbuilder_activate /path/to/promptbuilder` afterwards.
#
# Defines promptbuilder_activate / promptbuilder_restore so you can toggle
# back and forth as many times as you like in this shell, and activates it
# once right away. Only changes PROMPT_COMMAND/PS1 for this one session -
# your ~/.bashrc.d setup is untouched, and any new shell still gets the old
# prompt.
#
# Do NOT run this file directly - it has to be sourced so the
# PROMPT_COMMAND/PS1 changes apply to your interactive shell rather than a
# throwaway subshell.

_promptbuilder_repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

promptbuilder_activate() {
    local bin=${1:-$_promptbuilder_repo/promptbuilder}
    [[ $bin == /* ]] || bin=$PWD/$bin
    if [[ ! -x $bin ]]; then
        echo "promptbuilder: $bin not found - run 'make build' first" >&2
        return 1
    fi

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
    # The binary is called by its absolute path, never looked up in $PATH.
    #
    # The output goes into a variable that PS1 only *references*: bash
    # expands $(...) and `...` inside PS1 itself on every render, so
    # putting the output into PS1 directly would execute any such text in
    # a directory or branch name. A variable's value is not expanded again.
    PROMPT_COMMAND="_pb_exit=\$?; history -a; _pb_ps1=\$($(printf '%q' "$bin") \"\$_pb_exit\")"
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

# No "$@": a sourced file inherits the caller's positional parameters
# when given no arguments of its own.
promptbuilder_activate
