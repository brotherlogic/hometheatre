#!/bin/bash

# Ensure the 'hometheatre' session exists
if ! tmux has-session -t hometheatre 2>/dev/null; then
  # Create a new session named 'hometheatre', detached
  cd /workspaces/hometheatre
  tmux new-session -d -s hometheatre
  
  # Split the window horizontally (-h)
  # The left pane will remain a terminal
  # The right pane will run 'gh dash'
  tmux split-window -h -t hometheatre "gh dash"
fi
