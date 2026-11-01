#!/usr/bin/env bash
# Run upstream's real-CPA end-to-end suite (scripts/e2e_cpa_billing.sh) against
# the CPA Suite build, without modifying the upstream script: a temporary copy
# is rewritten to the cpa-suite plugin ID and config key.
#   scripts/e2e_suite.sh /path/to/CLIProxyAPI-source-or-binary
set -euo pipefail
cd "$(dirname "$0")/.."
tmp_script="scripts/.e2e_suite_generated.sh"
tmp_config="scripts/.e2e_suite_config.yaml"
trap 'rm -f "$tmp_script" "$tmp_config"' EXIT
sed -e 's#plugins/cpa-key-billing/#plugins/cpa-suite/#g' \
    -e 's#plugins/cpa-key-billing\.\$plugin_extension#plugins/cpa-suite.$plugin_extension#' \
    -e 's#select(.id == "cpa-key-billing"#select(.id == "cpa-suite"#' \
    -e 's#e2e_config.yaml#.e2e_suite_config.yaml#g' \
    scripts/e2e_cpa_billing.sh > "$tmp_script"
sed -e 's#^    cpa-key-billing:#    cpa-suite:\n      import_from: none\n      plugins_dir: "__RUNTIME_DIR__/plugins"#' scripts/e2e_config.yaml > "$tmp_config"
chmod +x "$tmp_script"
"$tmp_script" "$@"
