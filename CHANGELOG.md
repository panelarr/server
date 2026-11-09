# Changelog

## 0.1.0 (2026-11-09)


### ⚠ BREAKING CHANGES

* **api:** GET /api/v1/sources and /api/v1/sources/{source_id}/ {popular,latest,search,filters,series,chapters} answer 403 forbidden to non-admins. This is the one deliberate exception to additive-only changes within v1, recorded in docs/api.md.

### Features

* **api:** additive follow-ups for API clients ([2fd70b1](https://github.com/panelarr/server/commit/2fd70b14958f960f7e6eb4422d08001788670c14))
* **api:** make sources admin-only ([4a51c86](https://github.com/panelarr/server/commit/4a51c8657891baefa7f61df58259371a19075ef3))
* **api:** the time zone of cron job schedules ([bc123df](https://github.com/panelarr/server/commit/bc123df558bb5a73aa82af95ed2e600bed8e518a))
* **auth:** accounts, sessions, API keys, invites and first-run setup ([4aadeec](https://github.com/panelarr/server/commit/4aadeeced215ce27df04b00a43667de562294e70))
* **chapters:** read volumes, clean titles and extras from chapter names ([6116bde](https://github.com/panelarr/server/commit/6116bde8b0b8b0f086b8850a6628eed36be0df89))
* **chapters:** rebuild every work's chapters by hand ([1cdea4f](https://github.com/panelarr/server/commit/1cdea4fa9ac1ba6303339a722d9dc62bbea86054))
* **chapters:** unified chapters across sources and update checks ([a114382](https://github.com/panelarr/server/commit/a114382abb5c65511e4157125af0e0fb3209f91b))
* **downloads:** storage endpoints and CBZ downloads into the shared library ([8179f54](https://github.com/panelarr/server/commit/8179f548802840f209e153e041e7f3a90573abdf))
* **jobs:** say why a job can't run now ([a198416](https://github.com/panelarr/server/commit/a198416c6b1dfc98726488b960d0d205a158b8e1))
* **jobs:** task queue, named jobs and the event stream ([1f47386](https://github.com/panelarr/server/commit/1f473861f37c680fd8aa7988bc24e41b5f2319ee))
* **library:** reading stats on library entries ([ac8875e](https://github.com/panelarr/server/commit/ac8875e8eef6cedd1bbe442f4a77cdc24b6a6826))
* **library:** updates feed, check status and library refresh ([58c4196](https://github.com/panelarr/server/commit/58c4196220a627e8de1cfd2dcdafc1351200ee4e))
* **library:** works, libraries and matching works to source series ([a4b98e9](https://github.com/panelarr/server/commit/a4b98e983e5a67107116393bba1686d8aa24c682))
* **meta:** connect the metadata service and serve its catalogue ([7643a8a](https://github.com/panelarr/server/commit/7643a8ae529d9e5d6a2ed0dc53b948167be6cca3))
* **meta:** duplicate entries review in Panelarr's admin ([ef6790d](https://github.com/panelarr/server/commit/ef6790d2fd90f0809303151d965032902d44517e))
* **meta:** pass meta's job disabled_reason through ([38f9b9f](https://github.com/panelarr/server/commit/38f9b9fa1755655fe0c905cc6975aadfccb7f32b))
* **meta:** providers, sync runs and meta's health in Panelarr's admin ([8b282b2](https://github.com/panelarr/server/commit/8b282b2bcf2cd3266f7f0b6f647cfb497e8d676c))
* **meta:** the metadata service's jobs in Panelarr's admin ([04cfa71](https://github.com/panelarr/server/commit/04cfa71753dad805e3de59342831a6915be59aa9))
* **meta:** the search engine in Panelarr's admin ([3358fa5](https://github.com/panelarr/server/commit/3358fa5f7bc65192a1c318e8664abfc19c027f2e))
* **network:** custom solvers and proxies per plugin ([23b9ac1](https://github.com/panelarr/server/commit/23b9ac1c137bccd25f3c0e13256d16de46be019e))
* **network:** network profiles and where they are assigned ([65e652f](https://github.com/panelarr/server/commit/65e652f314110db8de868420d2b2a3f1ee5eacd8))
* **network:** network profiles for metadata providers ([52e3169](https://github.com/panelarr/server/commit/52e31698804e20474092f49bf85c09a53fca543b))
* **network:** set up solvers and proxies once in Panelarr ([d334cad](https://github.com/panelarr/server/commit/d334cada3ed91ad0194d9cf613689dd0b870419f))
* **plugins:** a plugin's own jobs in Panelarr's admin ([75e83a1](https://github.com/panelarr/server/commit/75e83a1f6f874d6fb42fa3f1262929d46aa0b8f4))
* **plugins:** check a plugin shortly after its settings are saved ([6e9e55a](https://github.com/panelarr/server/commit/6e9e55a4dc10bed06c83a8dfac3ee9486a609552))
* **plugins:** host remote source plugins and browse their sources ([b57831a](https://github.com/panelarr/server/commit/b57831ae86d56ab1c7594f4e8f608f56a5e10e24))
* **plugins:** image retry settings per extension and source ([1217f9d](https://github.com/panelarr/server/commit/1217f9d4cdc5afec6721029e2c87376bf20c7708))
* **plugins:** install packages and choose their languages from the admin ([1d732f6](https://github.com/panelarr/server/commit/1d732f63c16475dbf59b969c1813f8256d19f35a))
* **plugins:** package origins, details, reload and source health checks ([f5a6fe2](https://github.com/panelarr/server/commit/f5a6fe24d64986db6b0558440906bf9a18d0154c))
* **plugins:** push network profiles to plugins ([380aeb5](https://github.com/panelarr/server/commit/380aeb51d592b86e3503c50369c8b162d1586525))
* **plugins:** retry page images that fail for errors that may pass ([0913f34](https://github.com/panelarr/server/commit/0913f345d493e3a079d11fd19c246e5bd259c7d9))
* **plugins:** serve package icons and test package failures ([55c0fcd](https://github.com/panelarr/server/commit/55c0fcd930fd2d3215aca06e5cd4b640a5c6f1c1))
* **reader:** chapter bookmarks ([7e2aeee](https://github.com/panelarr/server/commit/7e2aeeed33ac088ac305a99507589849dbd736b1))
* **reader:** history per series, search, remove and clear ([4b30d31](https://github.com/panelarr/server/commit/4b30d3154d1e720103d8adcec9817dc62cdbed7a))
* **reader:** name the chapter and its work when pages fail ([f227bf0](https://github.com/panelarr/server/commit/f227bf0d415f0dedbefd2aa82f254bad59642f92))
* **reader:** pages, progress, history and reader settings ([e2726f1](https://github.com/panelarr/server/commit/e2726f1cbda68015caf9349b8d309ba2dcd3bac3))
* **sources:** a source's own settings through the API ([b3e9c0d](https://github.com/panelarr/server/commit/b3e9c0d5e79888b3023b24dca5c64dc029362bb7))
* **sources:** keep works on usable sources and search a work's source ([5afb3f2](https://github.com/panelarr/server/commit/5afb3f2b205659133d2d046bf9ff3f6bfbaf42d4))
* **web:** serve the web app and harden API responses ([a64e0cf](https://github.com/panelarr/server/commit/a64e0cfb19e410bbeb8f0d177d9ef8ee658e8663))


### Bug Fixes

* **api:** a client going away isn't a server error ([b679214](https://github.com/panelarr/server/commit/b6792142b92d8ca9553fa6240e373e6e7dd735ff))
* **api:** keep upstream 5xx as errors outside the image routes ([3eb4054](https://github.com/panelarr/server/commit/3eb4054478fcad9c602b15caaf94dea0df6411ad))
* **chapters:** more release tags out of display titles ([71b435f](https://github.com/panelarr/server/commit/71b435f396691bf1f29379658f3169d5ea22b1f7))
* **ci:** check out meta and source-api next to server; Go 1.27.3 ([40d623e](https://github.com/panelarr/server/commit/40d623e03cad89480e8a971d721ef88c6948e6fd))
* **ci:** check out meta and source-api next to server; Go 1.27.3 ([c894405](https://github.com/panelarr/server/commit/c89440511cee7c68313fbcfbea4efce4251d2604))
* **jobs:** don't accept cancelling a run that just finished ([6fcf9fc](https://github.com/panelarr/server/commit/6fcf9fc31368d45b76132505d2a50e912581ce58))
* **meta:** keep a cancelled request from spoiling the meta check ([97055ae](https://github.com/panelarr/server/commit/97055ae7e559bd54ad9b24682c373604ac0dba7b))
* **meta:** no time zone on interval schedules written as [@every](https://github.com/every) ([c29f229](https://github.com/panelarr/server/commit/c29f22913b918666566d3c32b4f8cd61249cab5a))
* **meta:** tell a down metadata service from one that answered ([0fd607b](https://github.com/panelarr/server/commit/0fd607b91b53f09459494eeb1ee74bf83686e3d2))
* **plugins:** connect every plugin before waiting on any at startup ([536d57c](https://github.com/panelarr/server/commit/536d57c366337fc82431e659d312fc6c937093cf))
* **plugins:** don't lose a network change made during a push ([b9e070d](https://github.com/panelarr/server/commit/b9e070d1475dbb5344751276c5528921f6df6988))
* **plugins:** recheck a plugin found down instead of waiting for the next sync ([6fc6819](https://github.com/panelarr/server/commit/6fc6819a777ccf1020321cded8473824a058e14e))
* **plugins:** wait out a startup window before marking a plugin down ([78fb718](https://github.com/panelarr/server/commit/78fb7180d07d4ab04872696d7ff51abf4a8da712))
* **plugins:** wait out a startup window before marking a plugin down ([6701028](https://github.com/panelarr/server/commit/67010285b468420c041bc59bf359e7b7c0d1519d))
* **reader:** page images use the plugin's image limits ([c6e8d27](https://github.com/panelarr/server/commit/c6e8d27dd2870c5c900b8761724ae73fb9ae78fd))
* **sources:** fall back to the package icon and quiet missing icons ([30a7566](https://github.com/panelarr/server/commit/30a7566edd9e8506ab79d00ac46eb83d411e9838))
* **sources:** fetch source icons from the plugin's address ([95fb5a7](https://github.com/panelarr/server/commit/95fb5a7ae06be55e6c7dcc645b6ff673c8f4336e))
