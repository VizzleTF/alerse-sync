# Privacy policy of sync.alerse.app

What the Alerse sync server at sync.alerse.app keeps about you, why, for how long, and
how to have it removed. In force from 2026-11-13. A server you run yourself is yours:
this policy does not cover it.

## In short

- Everything Alerse syncs is encrypted on your device before it is sent. The server
  stores it as data it cannot open: it never has your 24 words, your password or your
  account key.
- The server keeps only what it needs to sync and to fend off abuse: account and device
  records, timestamps, and for a few days the IP addresses requests came from.
- No cookies, no analytics, no advertising, nothing sold or shared for marketing.

## Who is responsible

The server is run by the maintainer of Alerse, VizzleTF on GitHub: the controller of
this data under the GDPR. Contact:
[open a private report](https://github.com/VizzleTF/alerse-sync/security/advisories/new)
in the alerse-sync repository; say in its title that it is a privacy request.

## What is kept, why and for how long

| Data | Why | Legal basis | Kept |
|---|---|---|---|
| Account: an ID derived from your 24 words, a salt and key-derivation settings, an Argon2id hash of a key derived from your password, your account key encrypted on your device | to let your devices sign in | contract (GDPR Art. 6(1)(b)) | until you delete the account |
| Devices: each device's public key, its name (your computer's name, such as "Anna's MacBook"), when it was added and last seen (to 5 minutes) | to show your devices and let you remove one | contract | until removed; then 90 days, so a removed key cannot rejoin |
| Synced data: bookmarks, open tabs, Spaces, passwords, history, settings, encrypted on your device; the server sees only its size, data type and timestamps | the service itself | contract | until you delete it or the account. Deleted items: 60 days. History: 90 days. A Space no device has contacted for a year is removed |
| IP address, approximate location (country, region, city from the IP), time, requested path, status and size of each request | to run the service and to block abuse | legitimate interest in security (Art. 6(1)(f)) | 1 day in the access log of the server's front proxy, deleted hourly, so at most about 25 hours. The proxy adds the location itself and cannot be told not to |
| IP address of a client the server shuts out for sending refused requests | to block abuse | legitimate interest | 7 days in the server's logs |
| IP addresses of failed TLS connections | to diagnose and stop attacks | legitimate interest | in the proxy's error log, which keeps its last 1 MB: a few days |
| Counters of requests, without IP addresses or accounts | to run the service | legitimate interest | 30 days |

Backups of the database are taken daily and kept 7 days, so deleted data leaves the
backups within 7 days.

## Where it is

The server, its database and its backups run in the European Union. Requests reach it
through a proxy on a server rented in Finland (H2NEXUS Cloud Services), where the TLS
connection ends. Nothing is transferred outside the European Union.

Other parties see only this:

- Cloudflare answers DNS queries for alerse.app; it does not carry the requests.
- Let's Encrypt issues the TLS certificate; its public logs list the name sync.alerse.app.
- The hosting provider in Finland runs the machine the proxy is on.

## Your rights

- **Delete your data**: Sync in the Alerse menu, Delete account, with your password.
  The account, its devices and all synced data are deleted at once, and from the
  backups within 7 days.
- **Remove a device**: Sync, Devices, with your password.
- **Access, correction, portability, restriction and objection**: ask through the
  contact above. Synced data is encrypted with keys only your devices hold, so your
  devices already have it in readable form; the server can give you the account and
  device records, and the log lines with your IP address while they are still kept.
- **Complain** to the data protection authority of your country
  ([list of EU authorities](https://www.edpb.europa.eu/about-edpb/about-edpb/members_en)).

Requests are answered within one month.

## Not covered

- What Alerse does on your device, such as checking GitHub for updates: the
  [Alerse README](https://github.com/VizzleTF/alerse#privacy) describes it.
- Sites you visit with Alerse, and search engines you use.

The service is not meant for children under 16.

## Changes

Changes to this policy are made in this file; its history on GitHub shows each one.
A change that keeps more data or keeps it longer takes effect 30 days after it is
published here.
