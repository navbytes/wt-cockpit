# Packaging: run wtd as a background service

`wtd` is meant to be left running for weeks, not started by hand every time.
These unit files start it at login and restart it if it crashes.

Both assume the `wtd` binary lives at `/usr/local/bin/wtd`:

```sh
make build
sudo cp bin/wtd /usr/local/bin/wtd
```

(adjust the `ProgramArguments`/`ExecStart` path in the unit file if you'd
rather install it elsewhere). Per-repo settings (roots, base branch,
guardrails) are expected to live in `~/.config/wtcockpit/config.toml` (see
the top-level README) — add flags to the unit file instead if you'd rather
not use a config file.

## macOS (launchd)

1. The plist's log paths use a `YOUR_USER` placeholder — launchd does not
   expand `~`, so fill in your actual home directory before loading it:

   ```sh
   sed -i '' "s#/Users/YOUR_USER#$HOME#" packaging/com.wtcockpit.wtd.plist
   mkdir -p "$HOME/Library/Logs/wtcockpit"
   ```

2. Install and load it:

   ```sh
   cp packaging/com.wtcockpit.wtd.plist ~/Library/LaunchAgents/
   launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.wtcockpit.wtd.plist
   ```

   `launchctl bootstrap gui/$UID <path>` is the modern replacement for the
   deprecated `launchctl load`.

3. Manage it:

   ```sh
   launchctl kickstart -k gui/$UID/com.wtcockpit.wtd   # restart
   launchctl bootout gui/$UID/com.wtcockpit.wtd        # stop + unload
   tail -f ~/Library/Logs/wtcockpit/wtd.log            # logs
   ```

## Linux (systemd, user unit)

1. Install it:

   ```sh
   mkdir -p ~/.config/systemd/user
   cp packaging/wtd.service ~/.config/systemd/user/
   ```

2. Enable and start it now:

   ```sh
   systemctl --user enable --now wtd.service
   ```

3. Manage it:

   ```sh
   systemctl --user status wtd.service
   systemctl --user restart wtd.service
   journalctl --user -u wtd.service -f   # logs (stdout/stderr go to journald)
   ```

   Run `loginctl enable-linger $USER` if you want wtd to keep running after
   you log out — user units otherwise stop when your last session ends.
