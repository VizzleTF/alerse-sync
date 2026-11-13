# Security policy

Report a vulnerability in Alerse or its sync server privately, through GitHub:
open the repository's **Security** tab and choose **Report a vulnerability**.
Do not open a public issue for it.

| Where the problem is | Report it in |
|---|---|
| The browser: the sidebar, Spaces, the command bar, import, the sync client | [VizzleTF/alerse](https://github.com/VizzleTF/alerse/security/advisories/new) |
| The sync server or sync.alerse.app | [VizzleTF/alerse-sync](https://github.com/VizzleTF/alerse-sync/security/advisories/new) |
| A build or installer of one platform | that platform's repository |
| Chromium itself, reproducible in plain Chromium | [Chromium's tracker](https://www.chromium.org/Home/chromium-security/reporting-security-bugs/) |

A report helps most with the version (Alerse menu, About), the platform and the
steps to reproduce it. A fix ships in the next
release; the advisory is published once it is out.

Supported: the latest release of each platform. Alerse follows Chromium's
security releases with a delay of days to weeks, since each one is rebuilt here.
