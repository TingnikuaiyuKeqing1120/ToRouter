package desktopui

import (
	"os/exec"
	"testing"
)

func TestBootstrapNotifiesAfterReadonlyMarkerAndRoute(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to execute bootstrap JavaScript")
	}
	for _, pet := range []bool{true, false} {
		script := `
const assert=require('node:assert/strict');
let notifications=0;
global.document={documentElement:{classList:{add(){}}}};
global.window={location:{hash:'#/'},dispatchEvent(event){
 assert.equal(event.type,'to-router-desktop-ready');
 assert.ok(window.__TO_ROUTER_DESKTOP__);
 assert.equal(window.__TO_ROUTER_DESKTOP__.origin,'http://127.0.0.1:3823');
 assert.ok(Object.isFrozen(window.__TO_ROUTER_DESKTOP__));
 assert.equal(Object.getOwnPropertyDescriptor(window,'__TO_ROUTER_DESKTOP__').writable,false);
 if(window.__TO_ROUTER_DESKTOP__.pet)assert.equal(window.location.hash,'#/pet');
 notifications++;
}};
` + bootstrap(pet, "http://127.0.0.1:3823") + bootstrap(pet, "http://127.0.0.1:3823") + `
assert.equal(notifications,2);
`
		if output, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
			t.Fatalf("bootstrap pet=%v: %v\n%s", pet, err, output)
		}
	}
}
