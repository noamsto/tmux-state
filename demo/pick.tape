# Browse earlier snapshots and see what each pane had on screen before choosing.
#   go build -o demo/bin/tmux-remux ./cmd/tmux-remux
#   nix shell nixpkgs#vhs -c vhs demo/pick.tape
Output demo/pick.gif

Set Shell fish
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 15
Set Width 1980
Set Height 800
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Padding 12

# Wider than undo.tape deliberately, and sized by the close picker, which is
# the binding constraint: it needs 163 columns to put the preview beside the
# list (the snapshot picker only needs 120), and the picker opens in a popup at
# 90% of the client — so 190 columns here leaves it 171, with margin over 163
# that 182 would not give. undo.tape's 95 could never show a preview at all.
Hide
Type "./demo/setup --cols 190 --rows 36 --history --demo-keys"
Enter
Sleep 45s
Type "clear; tmux -L vhs attach -t demo"
Enter
Sleep 3s
Show

Sleep 1500ms

# The snapshot picker. Keys land in the footer (REMUX_DEMO_KEYS via demo/setup),
# so the navigation is legible; vim keys j/k/l/h drive the tree.
Ctrl+b
Sleep 400ms
Type "R"
Sleep 2500ms

# Tab moves into the contents tree; the panes of a live session count as
# running, so the default filter hides them until d turns it off.
Tab
Sleep 1200ms
Type "d"
Sleep 1500ms

# j steps onto the window: the preview is a map of its pane layout, each pane
# drawn to scale and labelled with its index and command.
Type "j"
Sleep 3500ms

# l dives into the window — the preview becomes the first pane's scrollback.
Type "l"
Sleep 1800ms

# j walks to the agent's pane and the output it had on screen.
Type "j"
Sleep 3s
Escape
Sleep 800ms

# Now the other picker: close something first so there is something to browse.
# demo/setup handed focus back to the agent's pane, so this closes the one that
# has been logging for half a minute.
Ctrl+b
Sleep 400ms
Type "x"
Sleep 2500ms

# prefix+U lists what was closed: one flat row per close, newest first, under
# a heading for this session. There is no tree to walk and no Tab to press —
# the preview beside it is already showing the scrollback the closed pane had
# on screen, read out of the snapshot the close was diffed against.
Ctrl+b
Sleep 400ms
Type "U"
Sleep 4s

# Alt+k scrolls back through that output without leaving the row.
Alt+k
Sleep 700ms
Alt+k
Sleep 700ms
Alt+k
Sleep 3s

Hide
Escape
Sleep 500ms
Ctrl+b
Type ":kill-server"
Enter
