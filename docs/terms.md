# Terms of use of sync.alerse.app

The rules for using the Alerse sync server at sync.alerse.app. In force from
2026-11-13. A server you run yourself from this repository is covered by its license,
not by these terms.

## The service

- Prefer a server of your own: this repository runs in Docker or Kubernetes in a few
  minutes ([Run your own server](../README.md#run-your-own-server)), and your data stays
  with you.
- sync.alerse.app is there for those who would rather not run one. It is free, offered
  voluntarily by one person (VizzleTF on GitHub) in their own time, and holds a few
  accounts for now.
- The operator takes no responsibility for the data on sync.alerse.app: for its loss,
  its availability or anything that follows from it. It is encrypted on your devices,
  so the operator cannot read it either.
- It has no guaranteed uptime. It may be down, slow, limited or changed. It may be shut
  down; if so, it is announced in the
  [alerse-sync repository](https://github.com/VizzleTF/alerse-sync) at least 30 days
  ahead where possible, so you can move to a server of your own.
- What it keeps about you is in the [privacy policy](privacy.md).

## Your account

- Your 24 words and your password open the account. Nobody else has them, so nobody can
  reset them: if you lose both and every signed-in device, the synced data is gone.
- Keep the words and the password apart and safe. Anyone with both can sign in.
- You can delete the account at any time from Sync in the Alerse menu.

## Fair use

Use the server only to sync your own Alerse devices. Do not:

- send requests by other means than Alerse, or more than Alerse sends;
- try to reach data that is not yours, or to break the server's limits or security;
- store data for anyone else, or use the server as file storage.

Report a security problem privately, as
[SECURITY.md](https://github.com/VizzleTF/alerse/blob/main/SECURITY.md) explains.

The server enforces limits on its own. An account or device that breaks these rules may
be limited, removed or deleted without notice.

## No warranty

The service and its software are provided as they are, without warranty of any kind.
To the extent the law allows, the operator is not liable for lost data, lost access or
any damage from using the service. Keep a copy of what matters to you elsewhere: sync is
not a backup.

## Changes

Changes to these terms are made in this file; its history on GitHub shows each one, and
a change takes effect 30 days after it is published. Nothing in these terms limits the
protection the law of your country gives you as a consumer.
