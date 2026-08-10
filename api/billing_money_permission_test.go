package api

import (
	"testing"
)

// Editing the price list and putting money into somebody's account are not the
// same level of trust. This locks that separation in place: a future change
// that quietly folds the money operations back under admin.settings.write
// would silently hand every operator the ability to credit balances.
func TestMoneyPermissionIsNotGrantedToOperatorsByDefault(t *testing.T) {
	if allowed, present := defaultOperatorPermissions[permissionAdminBillingMoneyWrite]; !present {
		t.Fatal("money permission must appear in the operator defaults, otherwise its default is an accident of map lookup")
	} else if allowed {
		t.Fatal("operators must not hold admin.billing.money.write by default")
	}

	// The tier only means something if the ordinary write permission is still
	// something an operator can be granted separately.
	if _, present := defaultOperatorPermissions[permissionAdminSettingsWrite]; !present {
		t.Fatal("admin.settings.write must remain a distinct, separately-granted permission")
	}

	if permissionAdminBillingMoneyWrite == permissionAdminSettingsWrite {
		t.Fatal("the money permission must be a distinct string from admin.settings.write")
	}
}
