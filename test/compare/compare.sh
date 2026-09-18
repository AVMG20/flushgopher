#!/usr/bin/env bash
# Differential test: run the original cache-clean.js and flushgopher in watch
# mode against the same Magento install, make the same file edits, and record
# every redis command each tool sends (via MONITOR, with ECHO markers between
# steps). Also checks which generated classes each tool removes.
#
# usage: compare.sh <magento-dir> <redis-socket> <out-dir> <tool-name> <tool-cmd...>
# The Magento env.php must point both cache frontends at <redis-socket>.
set -u
WT=$1; SOCK=$2; OUT=$3; NAME=$4; shift 4
GAP=${GAP:-7}          # > 5s so the original's flood guard never suppresses a step
R() { redis-cli -s "$SOCK" "$@" >/dev/null; }
mkdir -p "$OUT"

files=(
  app/code/Acme/PdfTemplate/view/frontend/layout/pdf_order_items_table.xml
  app/code/Zenith/SecondBlogSitemap/view/adminhtml/templates/js.phtml
  app/code/Acme/PriceFix/etc/di.xml
  app/code/Nova/SearchCanonical/etc/frontend/di.xml
  app/code/Nova/InvoicePayment/etc/config.xml
  app/code/Nova/Personalization/etc/adminhtml/system.xml
  app/code/Orbit/ParcelCheckout/i18n/nl_NL.csv
  app/code/Nova/HeaderUsps/Block/Stylesheet.php
  app/code/Acme/CartRestore/ViewModel/RestoreQuote.php
  app/code/Acme/Cancellation/Api/CancelOrderLinesInterface.php
  app/code/Acme/MSI/etc/events.xml
  app/code/Acme/SearchAttributeLandingUrls/etc/frontend/events.xml
  app/code/Nova/AcmeReturns/etc/frontend/routes.xml
  app/code/Zenith/SecondBlogSitemap/etc/crontab.xml
  app/code/Acme/SalesRule/view/adminhtml/ui_component/sales_rule_form.xml
  app/design/frontend/Nova/theme-one/Magento_Theme/templates/theme-css-vars.phtml
  app/design/frontend/Nova/theme-two/Magento_Theme/layout/default.xml
  app/code/Acme/Swatches/view/frontend/requirejs-config.js
  app/code/Nova/AcmePersonalisation/etc/acl.xml
  app/code/Orbit/ParcelCheckout/etc/extension_attributes.xml
  app/code/Acme/Cancellation/etc/webapi.xml
  app/code/Nova/InvoicePayment/etc/email_templates.xml
  app/design/frontend/Nova/theme-two/web/images/logo.svg
  app/code/Nova/InvoicePayment/Model/LegacyPayAfterInvoice.php
  app/code/Nova/FeatureFlags/etc/frontend/sections.xml
  app/code/Pixel/HyvaCheckoutParcelShipping/etc/csp_whitelist.xml
  app/code/Acme/PosCleanup/etc/adminhtml/menu.xml
  app/code/Orbit/ParcelCheckout/etc/db_schema.xml
  app/design/frontend/Nova/theme-one/web/css/theme.css
  vendor/magento/module-catalog/etc/di.xml
  app/code/Nova/Lookbook/Controller/LookbookController.php
)
NEWCTRL=app/code/Nova/Lookbook/Controller/Fgtest/Index.php
DELETED=app/code/Acme/PdfTemplate/view/frontend/layout/pdf_order_items_table.xml

phpclass() { # fully qualified class of a php file
  local ns cl
  ns=$(grep -m1 -E '^\s*namespace\s' "$1" | sed -E 's/^\s*namespace\s+([^;]+);.*/\1/')
  cl=$(grep -m1 -E '^\s*((abstract|final|readonly)\s+)*(class|interface)\s' "$1" | sed -E 's/^\s*((abstract|final|readonly)\s+)*(class|interface)\s+(\w+).*/\4/')
  echo "$ns\\$cl"
}

# Seed generated classes for all edited php files.
GEN=$WT/generated/code
rm -rf "$GEN"; mkdir -p "$GEN"
: > "$OUT/generated-seeded.txt"
for f in "${files[@]}"; do
  [[ $f == *.php ]] || continue
  c=$(phpclass "$WT/$f"); p=${c//\\//}
  for g in "$p/Interceptor.php" "$p/Proxy.php" "${p}Factory.php" "${p%Interface}Extension.php" "${p%Interface}ExtensionInterface.php"; do
    mkdir -p "$(dirname "$GEN/$g")"; echo '<?php' > "$GEN/$g"; echo "$g" >> "$OUT/generated-seeded.txt"
  done
done
sort -u -o "$OUT/generated-seeded.txt" "$OUT/generated-seeded.txt"

VFILE=$WT/vendor/magento/module-catalog/etc/di.xml; cp -p "$VFILE" "$OUT/vendor-di.bak"

redis-cli -s "$SOCK" monitor > "$OUT/monitor.txt" &
MON=$!
sleep 0.5

"$@" > "$OUT/tool.log" 2>&1 < /dev/null &
TOOL=$!
sleep 8   # startup (the original shells out to php here)
R echo "MARK startup-done"

touchfile() {
  local f=$WT/$1
  case "$f" in
    *.php) printf '\n// fgtest %s\n' "$RANDOM" >> "$f" ;;
    *.xml|*.svg) printf '\n<!-- fgtest %s -->\n' "$RANDOM" >> "$f" ;;
    *.csv) printf '\n' >> "$f" ;;
    *) printf '\n/* fgtest %s */\n' "$RANDOM" >> "$f" ;;
  esac
}

for f in "${files[@]}"; do
  R echo "MARK $f"
  touchfile "$f"
  sleep "$GAP"
done

R echo "MARK new-controller"
mkdir -p "$(dirname "$WT/$NEWCTRL")"
printf '<?php\nnamespace Nova\\Lookbook\\Controller\\Fgtest;\nclass Index {}\n' > "$WT/$NEWCTRL"
sleep "$GAP"

R echo "MARK delete:$DELETED"
rm "$WT/$DELETED"
sleep "$GAP"

R echo "MARK same-content-rewrite"
# Rewrite a file with identical content and mtime (like a git checkout of an
# unchanged file or an editor re-saving): nothing needs cleaning.
f=$WT/app/code/Acme/PriceFix/etc/di.xml
cp -p "$f" "$f.fgtmp" && mv "$f.fgtmp" "$f"
sleep "$GAP"

R echo "MARK burst-50-edits-same-layout-file"
for i in $(seq 1 50); do touchfile app/design/frontend/Nova/theme-two/Magento_Theme/layout/default.xml; sleep 0.05; done
sleep 12

R echo "MARK end"
sleep 1
kill -INT $TOOL 2>/dev/null; sleep 1; kill -9 $TOOL 2>/dev/null
kill $MON 2>/dev/null
wait 2>/dev/null

(cd "$GEN" && find . -type f | sed 's|^\./||' | sort) > "$OUT/generated-left.txt"
comm -23 "$OUT/generated-seeded.txt" "$OUT/generated-left.txt" > "$OUT/generated-removed.txt"

# Reset the worktree.
cp -p "$OUT/vendor-di.bak" "$VFILE"
git -C "$WT" checkout -q -- app vendor 2>/dev/null
git -C "$WT" checkout -q -- . 2>/dev/null
rm -rf "$WT/$(dirname "$NEWCTRL")"
echo "$NAME done: $(wc -l < "$OUT/monitor.txt") redis commands"
