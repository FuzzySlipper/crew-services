# Desktop input on the Linux agent box

On den-agents an agent can drive the desktop session (Wayland, KDE Plasma) with real keyboard, mouse
and gamepad input, with no root and no approval. Use it for a native window that has no page or
debug route to drive: a shipped release, a desktop shell window, a dialog.

The Windows box has its own route: [Windows playtest box](playtest-windows.md#two-tiers-of-input).

## Why no permission is needed

The box is agent-only. The `agent` user has read and write access to `/dev/uinput` (an ACL; check with
`getfacl /dev/uinput`), and udev's `70-agent-input-uaccess.rules` lets the seat user read every
`/dev/input/event*`. So a process running as `agent` can create virtual input devices and read which
keys any device holds.

## agent-input

`scripts/agent-input` is one dependency-free Python file, linked as `~/.local/bin/agent-input`:

```sh
ln -sf /home/agent/dev/crew-services/scripts/agent-input ~/.local/bin/agent-input
```

Its first command starts a per-user daemon. The daemon owns a virtual keyboard and mouse
(`agent-input keyboard mouse`) and a virtual Xbox 360 pad, so SDL and the Engine see an ordinary pad.
Each command returns once its input has been delivered and released, so a script's commands run in order.

```sh
agent-input focus "Rusty Dagger" --fullscreen   # activate the window whose title contains this text
agent-input key tab                             # named key, held 80 ms
agent-input key w 1500                          # held 1.5 s
agent-input chord shift-left+tab                # keys held together
agent-input type "Nameless"                     # ASCII text, US layout
agent-input button a                            # pad button: a b x y lb rb back start guide ls rs up down left right
agent-input stick lx 1 800                      # pad stick: lx ly rx ry (-1..1, +y up), lt rt (0..1); 0 ms holds
agent-input move -150 0                         # relative pointer motion (mouse look when the pointer is locked)
agent-input click left                          # left, right, middle
agent-input wheel -2
agent-input shot /path/out.png                  # the active window; --screen for the whole screen
agent-input held                                # which keys every input device holds down
agent-input unlock                              # unlock the session if the screen locker took it
agent-input status | stop
```

Key names are the Engine's keyboard-control names where they exist (`arrow-up`, `shift-left`, `enter`,
`escape`, `f1`…`f12`, letters and digits), with the short aliases `up`, `down`, `left`, `right`, `esc`,
`ctrl`, `shift` and `alt`. `code:N` sends a raw Linux key code.

## Pitfalls

- **Input goes to the focused window.** Run `agent-input focus TITLE` first. Text typed with nothing
  focused lands in whatever window has focus, such as a terminal.
- **The screen locker takes focus during long runs.** A game then sees its menu open and stops seeing
  keys. Run `agent-input unlock`, then `agent-input focus` again.
- **A key that never arrives may be held down on another device.** For example, a remote-desktop
  keyboard that kept one key pressed. `agent-input held` lists every device's held keys.
- **Concurrent agents share one desktop and one focus.** Coordinate before driving a window someone
  else is using.
- **Window titles.** Use a KWin script to list them (`workspace.windowList()`, printing each `caption`
  to the journal), when you don't know the title.
- **The daemon's log** is `$XDG_RUNTIME_DIR/agent-input/log`.
