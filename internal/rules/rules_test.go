package rules

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCacheTypes(t *testing.T) {
	cases := map[string][]string{
		"/m/Vendor/Mod/etc/di.xml":                        {"config"},
		"/m/Vendor/Mod/etc/frontend/di.xml":               {"config"},
		"/m/Vendor/Mod/etc/config.xml":                    {"config"},
		"/m/Vendor/Mod/view/frontend/layout/default.xml":  {"layout", "full_page"},
		"/m/Vendor/Mod/view/adminhtml/ui_component/f.xml": {"layout", "full_page"},
		"/m/Vendor/Mod/i18n/nl_NL.csv":                    {"translate", "full_page"},
		"/m/Vendor/Mod/view/frontend/templates/x.phtml":   {"block_html", "full_page"},
		"/m/Vendor/Mod/Block/Foo.php":                     {"block_html", "full_page"},
		"/m/Vendor/Mod/etc/adminhtml/menu.xml":            {"config", "block_html"},
		"/m/Vendor/Mod/etc/frontend/sections.xml":         {"full_page"},
		"/m/Vendor/Mod/view/frontend/requirejs-config.js": {"full_page"},
		"/m/Vendor/Mod/etc/csp_whitelist.xml":             {"full_page"},
		"/m/theme/web/images/logo.svg":                    {"hyva_svg"},
		"/m/Vendor/Mod/etc/view.xml":                      {"block_html", "full_page"},
		"/m/Vendor/Mod/Model/Foo.php":                     nil,
		"/m/Vendor/Mod/web/css/styles.css":                nil,
		"/m/Vendor/Mod/etc/db_schema.xml":                 nil,
	}
	for file, want := range cases {
		got := CacheTypes(file)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", file, got, want)
		}
	}
}

func TestCacheIDs(t *testing.T) {
	cases := map[string][]string{
		"/m/V/M/etc/adminhtml/system.xml":                        {"adminhtml__backend_system_configuration_structure"},
		"/m/V/M/etc/adminhtml/system/x.xml":                      {"adminhtml__backend_system_configuration_structure"},
		"/m/V/M/etc/events.xml":                                  {"global__event_config_cache"},
		"/m/V/M/etc/frontend/events.xml":                         {"frontend__event_config_cache"},
		"/m/V/M/etc/frontend/routes.xml":                         {"frontend::RoutesConfig"},
		"/m/V/M/etc/crontab.xml":                                 {"crontab_config_cache"},
		"/m/V/M/etc/webapi.xml":                                  {"webapi_config"},
		"/m/V/M/etc/acl.xml":                                     {"provider_acl_resources_cache"},
		"/m/V/M/etc/extension_attributes.xml":                    {"extension_attributes_config"},
		"/m/V/M/etc/di.xml":                                      {"resolvers"},
		"/m/V/M/view/frontend/layout/a.xml":                      {"resolvers"},
		"/m/V/M/view/adminhtml/ui_component/product_form.xml":    {"ui_component_configuration_data_product_form"},
		"/m/V/M/view/adminhtml/pagebuilder/content_type/row.xml": {"pagebuilder_config", "content_type_row"},
		"/m/V/M/etc/frontend/sections.xml":                       {"sections_invalidation_config"},
	}
	for file, want := range cases {
		got := CacheIDs(file)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", file, got, want)
		}
	}
}

func TestServiceContractIDs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "Vendor/Mod/Api/FooRepositoryInterface.php")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte(`<?php
namespace Vendor\Mod\Api;

interface FooRepositoryInterface
{
    public function save(FooInterface $foo);
    public function getById(int $id);
}
`), 0o644)
	got := CacheIDs(f)
	want := []string{
		"service_method_params_" + md5hex(`Vendor\Mod\Api\FooRepositoryInterface`+"save"),
		"service_method_params_" + md5hex(`Vendor\Mod\Api\FooRepositoryInterface`+"getById"),
		"serviceInterfaceMethodsMap-" + md5hex(`Vendor\Mod\Api\FooRepositoryInterface`),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestViewModelIsTemplate(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "Vendor/Mod/ViewModel/Thing.php")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("<?php\nnamespace Vendor\\Mod\\ViewModel;\nuse Magento\\Framework\\View\\Element\\Block\\ArgumentInterface;\nclass Thing implements ArgumentInterface {}\n"), 0o644)
	if got := CacheTypes(f); !slices.Equal(got, []string{"block_html", "full_page"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPHPClass(t *testing.T) {
	src := []byte("<?php\ndeclare(strict_types=1);\n\nnamespace Vendor\\Mod\\Model;\n\nfinal readonly class Foo extends Bar\n{\n}\n")
	if got := PHPClass(src); got != `\Vendor\Mod\Model\Foo` {
		t.Fatalf("got %q", got)
	}
}

func TestContainsJSTranslation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	if !ContainsJSTranslation(write("a.html", `<span translate="'Hi'"/>`)) {
		t.Error("html translate attribute")
	}
	if !ContainsJSTranslation(write("b.phtml", `<?= $t('x') ?>`)) {
		t.Error("phtml $t(")
	}
	if ContainsJSTranslation(write("c.js", `console.log(1)`)) || !ContainsJSTranslation(write("d.js", `$.mage.__("x")`)) {
		t.Error("plain js")
	}
	if !ContainsJSTranslation("/x/i18n/en_US.csv") {
		t.Error("csv")
	}
}

func TestHelpers(t *testing.T) {
	if !IsController("/m/V/M/Controller/Index/Index.php") || IsController("/m/V/M/Model/Controller.php") {
		t.Error("IsController")
	}
	if DiArea("/m/V/M/etc/frontend/di.xml") != "frontend" || DiArea("/m/V/M/etc/di.xml") != "" {
		t.Error("DiArea")
	}
}
