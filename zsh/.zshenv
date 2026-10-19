# 由程序通过 ZDOTDIR 指向本目录。.zshenv 在 /etc/zsh/* 之前加载，
# 所以这里的设置能影响系统级启动文件。
#
# Debian/Ubuntu 的 /etc/zsh/zshrc 默认会调用 compinit。如果 $fpath 里有
# 权限不安全的目录（例如 Homebrew 的 site-functions），compinit 会停下来
# 交互提问「Ignore insecure directories ... [y/n]?」，bot 的 shell 就永远
# 到不了提示符。补全对聊天驱动的会话没有意义，直接跳过全局 compinit。
skip_global_compinit=1

# 如果装了 oh-my-zsh，让它的 compinit 也跳过权限检查（兜底）。
export ZSH_DISABLE_COMPFIX=true
