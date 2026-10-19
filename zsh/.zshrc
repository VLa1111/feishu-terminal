# 飞书终端 bot 专用 zshrc（程序通过 ZDOTDIR 指向本目录）
export DISABLE_AUTO_UPDATE="true"
export DISABLE_AUTO_TITLE="true"
export ZSH_DISABLE_COMPFIX="true"
export HISTFILE=/dev/null
export SAVEHIST=0

# 安装了 oh-my-zsh 则加载（提供 git 别名/插件等），否则退回纯 zsh
if [[ -d "$HOME/.oh-my-zsh" ]]; then
  export ZSH="$HOME/.oh-my-zsh"
  export ZSH_THEME=""   # 不用自带主题，改用下方自定义「地址栏」提示符
  source "$ZSH/oh-my-zsh.sh"
fi

# git 分支信息（不在 git 仓库内则输出空）
__feishu_git_branch() {
  local ref
  ref=$(git symbolic-ref --short HEAD 2>/dev/null)
  [[ -n "$ref" ]] && echo " ($ref)"
}

# 地址栏：当前目录 + git 分支 + 提示符。
# 末尾的 FEISHU_SENTINEL 由程序注入，用于判断「命令已执行完毕」，会被从输出中剥掉。
#
# PROMPT_SUBST 必须打开：zsh 默认只在「给 PROMPT 赋值」那一刻做一次命令替换，
# 之后每次显示提示符只展开 % 转义。缺了它，单引号里的 $(__feishu_git_branch)
# 不会被求值，而是作为字面量印在每条输出前面。
setopt PROMPT_SUBST
PROMPT='%~$(__feishu_git_branch) %# '${FEISHU_SENTINEL}

# 关闭 ZLE 行编辑器，避免它向 PTY 输出 \b / \x1b> 等重绘乱码污染命令输出
unsetopt ZLE
# 关闭「部分行」% 标记（PROMPT_SP），避免每条输出后多一个孤立的 % 行
unsetopt PROMPT_SP
