// Package rules maps a changed file to the Magento cache types and cache ids
// that need to be cleaned. Ported from fingerprint_file.cljs and cache.cljs of
// the original magento-cache-clean, except that a file matching several rules
// gets the union of all of them (the original picked an arbitrary one).
package rules

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type rule struct {
	re    *regexp.Regexp
	types []string
}

type idRule struct {
	re  *regexp.Regexp
	ids []string
}

var (
	config      = []string{"config"}
	layout      = []string{"layout", "full_page"}
	translation = []string{"translate", "full_page"}
	template    = []string{"block_html", "full_page"}
	menu        = []string{"config", "block_html"}
	fpc         = []string{"full_page"}
	svg         = []string{"hyva_svg"}
)

func r(pattern string, types []string) rule { return rule{regexp.MustCompile(pattern), types} }

var typeRules = []rule{
	r(`/etc(?:/[^/]+|)/di\.xml$`, config),
	r(`/etc/widget\.xml$`, config),
	r(`/etc/product_types\.xml$`, config),
	r(`/etc/product_options\.xml$`, config),
	r(`/etc/payment\.xml$`, config),
	r(`/etc/search_request\.xml$`, config),
	r(`/etc/config\.xml$`, config),
	r(`/etc/indexer\.xml$`, config),

	r(`/layout/.+\.xml$`, layout),
	r(`/ui_component/.+\.xml$`, layout), // because of htmlContent blocks
	r(`/page_layout/.+\.xml$`, layout),

	r(`/i18n/.+\.csv$`, translation),

	r(`/templates/.+\.phtml$`, template),
	r(`/etc/view\.xml$`, template),
	r(`/theme\.xml$`, template),
	r(`/Block/.+\.php`, template),

	r(`/etc/adminhtml/menu\.xml$`, menu),

	r(`/etc/frontend/sections\.xml$`, fpc), // section names list in head
	r(`/view/(?:base|frontend|adminhtml)/requirejs-config\.js$`, fpc),
	r(`/etc/csp_whitelist\.xml$`, fpc),
	r(`/etc/schema\.graphqls$`, fpc),

	r(`\.svg$`, svg),
}

func ir(pattern string, ids ...string) idRule { return idRule{regexp.MustCompile(pattern), ids} }

// idRules clean individual cache records instead of complete cache types,
// which keeps cache rebuild times short.
var idRules = []idRule{
	ir(`/etc/adminhtml/system.*\.xml$`, "adminhtml__backend_system_configuration_structure"),
	ir(`/etc/queue\.xml$`, "message_queue_config_cache"),
	ir(`/etc/queue_consumer\.xml$`, "message_queue_consumer_config_cache"),
	ir(`/etc/queue_publisher\.xml$`, "message_queue_publisher_config_cache"),
	ir(`/etc/queue_topology\.xml$`, "message_queue_topology_config_cache"),
	ir(`/etc/crontab\.xml$`, "crontab_config_cache"),
	ir(`/hyva_checkout\.xml$`, "checkout_config_cache", "hyva_checkout_config_cache"),
	ir(`/etc/adminhtml/(?:hyva_|)dashboard_widget\.xml$`, "hyva_admin_dashboard"),
	ir(`/etc/hyva_cms/components\.json$`, "hyva_cms"),
	ir(`/etc/events\.xml$`, "global__event_config_cache"),
	ir(`/etc/frontend/events\.xml$`, "frontend__event_config_cache"),
	ir(`/etc/adminhtml/events\.xml$`, "adminhtml__event_config_cache"),
	ir(`/etc/webapi_rest/events\.xml$`, "webapi_rest__event_config_cache"),
	ir(`/etc/webapi_soap/events\.xml$`, "webapi_soap__event_config_cache"),
	ir(`/etc/graphql/events\.xml$`, "graphql__event_config_cache"),
	ir(`/etc/crontab/events\.xml$`, "crontab__event_config_cache"),
	ir(`/etc/frontend/sections\.xml$`, "sections_invalidation_config"),
	ir(`/etc/email_templates\.xml$`, "email_templates"),
	ir(`/etc/webapi\.xml$`, "webapi_config"),
	ir(`/etc/schema\.graphqls$`, "magento_framework_graphqlschemastitching_config_data"),
	ir(`/etc/catalog_attributes\.xml$`, "catalog_attributes"),
	ir(`/etc/sales\.xml$`, "sales_totals_config_cache"),
	ir(`/etc/extension_attributes\.xml$`, "extension_attributes_config"),
	ir(`/etc/acl\.xml$`, "provider_acl_resources_cache"),
	ir(`/etc/frontend/routes\.xml$`, "frontend::RoutesConfig"),
	ir(`/etc/adminhtml/routes\.xml$`, "adminhtml::RoutesConfig"),
	ir(`/layout/.+\.xml$`, "resolvers"), // Magewire block name to resolver map
	ir(`/etc(?:/[^/]+|)/di\.xml$`, "resolvers"),
	ir(`/etc/csp_whitelist\.xml$`,
		"global::csp_whitelist_config", "frontend::csp_whitelist_config",
		"adminhtml::csp_whitelist_config", "webapi_rest::csp_whitelist_config",
		"webapi_soap::csp_whitelist_config", "graphql::csp_whitelist_config"),
}

var (
	uiComponentRe  = regexp.MustCompile(`/ui_component/(.+)\.xml$`)
	pageBuilderRe  = regexp.MustCompile(`/view/adminhtml/pagebuilder/content_type/(.*)\.xml`)
	controllerRe   = regexp.MustCompile(`/Controller/.+\.php$`)
	serviceIfaceRe = regexp.MustCompile(`/Api/.+Interface\.php$`)
	diAreaRe       = regexp.MustCompile(`/etc/([a-z_]+)/di\.xml$`)
)

const argumentInterface = `Magento\Framework\View\Element\Block\ArgumentInterface`

func slash(file string) string { return filepath.ToSlash(file) }

// CacheTypes returns the cache types to clean when file changed. PHP files
// are read to detect view models.
func CacheTypes(file string) []string {
	f := slash(file)
	var out []string
	for _, r := range typeRules {
		if r.re.MatchString(f) {
			out = append(out, r.types...)
		}
	}
	if strings.HasSuffix(f, ".php") && isViewModel(file) {
		out = append(out, template...)
	}
	return dedupe(out)
}

// CacheIDs returns the (un-prefixed) cache ids to clean when file changed.
func CacheIDs(file string) []string {
	f := slash(file)
	var out []string
	for _, r := range idRules {
		if r.re.MatchString(f) {
			out = append(out, r.ids...)
		}
	}
	if m := uiComponentRe.FindStringSubmatch(f); m != nil {
		out = append(out, "ui_component_configuration_data_"+m[1])
	}
	if m := pageBuilderRe.FindStringSubmatch(f); m != nil {
		out = append(out, "pagebuilder_config", "content_type_"+m[1])
	}
	out = append(out, serviceContractIDs(file)...)
	return dedupe(out)
}

func IsController(file string) bool { return controllerRe.MatchString(slash(file)) }

// DiArea returns the area of an area specific di.xml ("frontend", "adminhtml", ...).
func DiArea(file string) string {
	if m := diAreaRe.FindStringSubmatch(slash(file)); m != nil {
		return m[1]
	}
	return ""
}

func IsDiXML(file string) bool { return strings.HasSuffix(slash(file), "/di.xml") }

// ContainsJSTranslation reports whether a changed file may affect the
// compiled js-translation.json.
func ContainsJSTranslation(file string) bool {
	if strings.HasSuffix(file, ".csv") {
		return true
	}
	html := strings.HasSuffix(file, ".html")
	if !html && !strings.HasSuffix(file, ".phtml") && !strings.HasSuffix(file, ".js") {
		return false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	for _, s := range []string{"i18n:", ".mage.__(", "$t("} {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	if html {
		for _, s := range []string{"translate=", "translate args="} {
			if bytes.Contains(b, []byte(s)) {
				return true
			}
		}
	}
	return false
}

func isViewModel(file string) bool {
	head := Head(file, 2048)
	return bytes.Contains(head, []byte(argumentInterface))
}

// Head returns up to n bytes from the start of file (nil on error).
func Head(file string, n int) []byte {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	k, _ := f.Read(buf)
	return buf[:k]
}

var (
	phpNamespaceRe = regexp.MustCompile(`(?mi)^\s*namespace\s+([a-z0-9_\\]+)`)
	phpClassRe     = regexp.MustCompile(`(?mi)^\s*(?:(?:abstract|final|readonly)\s+)*(?:class|interface|trait|enum)\s+(\w+)`)
	phpInterfaceRe = regexp.MustCompile(`(?mi)^\s*interface\s+(\w+)`)
	phpMethodRe    = regexp.MustCompile(`\bpublic\s+(?:static\s+)?function\s+(\w+)\s*\(`)
)

// PHPClass returns the fully qualified class name (with leading backslash)
// declared in the PHP source, or "".
func PHPClass(src []byte) string {
	ns := phpNamespaceRe.FindSubmatch(src)
	cl := phpClassRe.FindSubmatch(src)
	if ns == nil || cl == nil {
		return ""
	}
	return `\` + string(ns[1]) + `\` + string(cl[1])
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// serviceContractIDs mirrors \Magento\Framework\Reflection\MethodsMap cache ids.
func serviceContractIDs(file string) []string {
	if !serviceIfaceRe.MatchString(slash(file)) {
		return nil
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	ns := phpNamespaceRe.FindSubmatch(src)
	iface := phpInterfaceRe.FindSubmatch(src)
	if ns == nil || iface == nil {
		return nil
	}
	name := string(ns[1]) + `\` + string(iface[1])
	var ids []string
	for _, m := range phpMethodRe.FindAllSubmatch(src, -1) {
		ids = append(ids, "service_method_params_"+md5hex(name+string(m[1])))
	}
	return append(ids, "serviceInterfaceMethodsMap-"+md5hex(name))
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
