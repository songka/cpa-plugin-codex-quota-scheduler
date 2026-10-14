# Source and dependency notices

Codex Quota Reserve is derived from JefferyZhang2019/cpa-plugin-codex-quota-scheduler v0.3.2, commit f27aa969ec726a5cc01bdbfcd588cae49306f452. The original MIT copyright and permission notice remain in LICENSE. Original English and Chinese notices remain under docs/upstream/.

The scheduler's upstream documentation attributes MIT contributions from doer-ee (model retry chain), dos1989 (managed quota disable/recovery) and Siriussee (probe hardening). Those notices and the inherited source are retained.

Runtime Go module dependencies:

- github.com/router-for-me/CLIProxyAPI/v8 v8.0.27: MIT, reproduced in licenses/CLIProxyAPI-MIT.txt.
- gopkg.in/yaml.v3 v3.0.1: MIT for libyaml-derived portions and Apache-2.0 for other portions, complete upstream license reproduced in licenses/yaml.v3-LICENSE.txt.
- The Go standard library/toolchain is distributed under its BSD-style license, reproduced in licenses/Go-LICENSE.txt.

The CPA host is used only as an external integration target and is not bundled in the plugin ZIP. Linux builds dynamically link the platform C runtime; use the host distribution's libc license notices for that separately installed system component.
