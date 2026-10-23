package infrastructure_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	authorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	oidcpersonneldirectory "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/personneldirectory"
	ownerdirectoryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/application"
	ownerdirectoryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/ownerdirectory/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestBootstrapAdministratorProtectionMySQL 在专用测试库中走完整迁移链，创建一次真实的
// 初始超级管理员，然后验证列表隐藏与所有写入守卫。未配置 PLATFORM_IDENTITY_TEST_DSN 时跳过；
// 该 DSN 必须指向专用测试数据库。
func TestBootstrapAdministratorProtectionMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_IDENTITY_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_IDENTITY_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open isolated test database: %v", err)
	}
	sqlDatabase, err := database.DB()
	if err != nil {
		t.Fatalf("read isolated test database handle: %v", err)
	}
	defer sqlDatabase.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	transaction := database.WithContext(ctx).Begin()
	if transaction.Error != nil {
		t.Fatalf("begin fixture transaction: %v", transaction.Error)
	}
	defer transaction.Rollback()

	var initialized int64
	if err := transaction.Table("iam_bootstrap_state").Count(&initialized).Error; err != nil {
		t.Fatalf("count bootstrap state: %v", err)
	}
	if initialized != 0 {
		t.Fatal("isolated test database is already initialized; refusing to modify its administrator")
	}

	repository, err := infrastructure.NewGORMRepository(transaction)
	if err != nil {
		t.Fatalf("NewGORMRepository: %v", err)
	}
	service, err := application.NewBootstrapService(repository, security.Argon2idPasswordHasher{}, ulid.Generator{}, application.SystemClock{})
	if err != nil {
		t.Fatalf("NewBootstrapService: %v", err)
	}
	admin, err := service.InitializeFirstSuperAdmin(ctx, application.BootstrapInput{
		DisplayName: "部署初始管理员",
		AccountName: "bootstrap-admin-test",
		Password:    "TestOnly-Passw0rd!2026",
	})
	if err != nil {
		t.Fatalf("InitializeFirstSuperAdmin: %v", err)
	}
	tenantID := admin.TenantID
	operatorID := admin.UserID

	identity, ok, err := bootstrapadmin.Resolve(ctx, transaction, tenantID)
	if err != nil || !ok {
		t.Fatalf("Resolve() = (%#v, %v, %v), want initialized identity", identity, ok, err)
	}
	if identity.UserID != admin.UserID || identity.AccountID != admin.AccountID {
		t.Fatalf("Resolve() identity = %#v, want user %s account %s", identity, admin.UserID, admin.AccountID)
	}

	users, err := repository.ListUsers(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if users.Total != 0 || len(users.Items) != 0 {
		t.Fatalf("ListUsers returned protected administrator: total=%d items=%#v", users.Total, users.Items)
	}
	accounts, err := repository.ListAccounts(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if accounts.Total != 0 || len(accounts.Items) != 0 {
		t.Fatalf("ListAccounts returned protected administrator: total=%d items=%#v", accounts.Total, accounts.Items)
	}
	memberships, err := repository.ListMemberships(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if memberships.Total != 0 || len(memberships.Items) != 0 {
		t.Fatalf("ListMemberships returned protected administrator: total=%d items=%#v", memberships.Total, memberships.Items)
	}
	searched, err := repository.ListUsers(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 50, Keyword: "部署初始管理员", Status: identitydomain.StatusActive})
	if err != nil {
		t.Fatalf("ListUsers(search): %v", err)
	}
	if searched.Total != 0 || len(searched.Items) != 0 {
		t.Fatalf("ListUsers(search) returned protected administrator: total=%d items=%#v", searched.Total, searched.Items)
	}

	user, err := repository.GetUser(ctx, tenantID, admin.UserID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	disabled := identitydomain.StatusDisabled
	renamed, err := repository.UpdateUser(ctx, application.UserUpdateInput{
		TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID, DisplayName: "部署初始管理员-改名",
		Version: user.Version,
	}, nil, nil)
	if err != nil {
		t.Fatalf("UpdateUser(rename) error = %v, want success", err)
	}
	if renamed.DisplayName != "部署初始管理员-改名" {
		t.Fatalf("renamed display name = %q", renamed.DisplayName)
	}
	if _, err := repository.UpdateUser(ctx, application.UserUpdateInput{
		TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID, DisplayName: renamed.DisplayName,
		Status: &disabled, Version: renamed.Version,
	}, nil, nil); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateUser(disable) error = %v, want ErrProtected", err)
	}
	expiry := time.Now().UTC().Add(24 * time.Hour)
	if _, err := repository.UpdateUser(ctx, application.UserUpdateInput{
		TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID, DisplayName: renamed.DisplayName,
		UpdateValidity: true, ValidUntil: &expiry, Version: renamed.Version,
	}, nil, nil); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateUser(expire) error = %v, want ErrProtected", err)
	}
	if err := repository.DeleteUser(ctx, application.UserDeleteInput{
		TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID, Version: renamed.Version,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("DeleteUser error = %v, want ErrProtected", err)
	}
	preserved, err := repository.GetUser(ctx, tenantID, admin.UserID)
	if err != nil {
		t.Fatalf("GetUser after rejected writes: %v", err)
	}
	if preserved.Status != identitydomain.StatusActive || preserved.ValidUntil != nil {
		t.Fatalf("protected user changed after rejected writes: %#v", preserved)
	}

	account, err := loadAccountFixture(transaction, tenantID, admin.AccountID)
	if err != nil {
		t.Fatalf("load account fixture: %v", err)
	}
	if _, err := repository.UpdateAccount(ctx, application.AccountUpdateInput{
		TenantID: tenantID, OperatorID: operatorID, AccountID: admin.AccountID, Status: disabled, Version: account.Version,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateAccount(disable) error = %v, want ErrProtected", err)
	}
	if _, err := repository.UpdateAccount(ctx, application.AccountUpdateInput{
		TenantID: tenantID, OperatorID: operatorID, AccountID: admin.AccountID, Status: identitydomain.StatusActive,
		UpdateValidity: true, ValidUntil: &expiry, Version: account.Version,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateAccount(expire) error = %v, want ErrProtected", err)
	}

	membership, err := loadMembershipFixture(transaction, tenantID, admin.UserID)
	if err != nil {
		t.Fatalf("load membership fixture: %v", err)
	}
	active := identitydomain.StatusActive
	if _, err := repository.UpdateMembership(ctx, application.MembershipUpdateInput{
		MembershipCreateInput: application.MembershipCreateInput{
			TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID,
			OrgUnitID: membership.OrgUnitID, PositionID: membership.PositionID, MembershipType: membership.MembershipType,
		},
		MembershipID: membership.ID, Status: &disabled, Version: membership.Version,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateMembership(disable) error = %v, want ErrProtected", err)
	}
	if _, err := repository.UpdateMembership(ctx, application.MembershipUpdateInput{
		MembershipCreateInput: application.MembershipCreateInput{
			TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID,
			OrgUnitID: membership.OrgUnitID, PositionID: membership.PositionID, MembershipType: membership.MembershipType,
			EffectiveTo: &expiry,
		},
		MembershipID: membership.ID, Status: &active, Version: membership.Version,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateMembership(expire) error = %v, want ErrProtected", err)
	}
	if err := repository.DeletePosition(ctx, application.PositionDeleteInput{
		TenantID: tenantID, OperatorID: operatorID, PositionID: membership.PositionID, Version: membership.PositionVersion,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("DeletePosition error = %v, want ErrProtected", err)
	}
	if err := repository.DeleteOrgUnit(ctx, application.OrgUnitDeleteInput{
		TenantID: tenantID, OperatorID: operatorID, OrgUnitID: membership.OrgUnitID, Version: membership.OrgUnitVersion,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("DeleteOrgUnit error = %v, want ErrProtected", err)
	}

	personnelChanges := infrastructure.NewPersonnelChangeGORMRepository(transaction)
	if err := personnelChanges.ValidateCreate(ctx, application.PersonnelChangeCreateInput{
		TenantID: tenantID, OperatorID: operatorID, UserID: admin.UserID,
		SourceMembershipID: membership.ID, TargetOrgUnitID: membership.OrgUnitID, TargetPositionID: membership.PositionID,
		ChangeType: identitydomain.PersonnelChangeTransfer, Reason: "隔离测试异动", EffectiveAt: time.Now().UTC().Add(time.Hour),
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("ValidateCreate(personnel change) error = %v, want ErrProtected", err)
	}
	scheduledID := "01JTESTPERSONNELCHANGE0001"
	if err := transaction.Table("iam_personnel_change_request").Create(map[string]any{
		"id": scheduledID, "tenant_id": tenantID, "user_id": admin.UserID,
		"source_membership_id": membership.ID, "target_org_unit_id": membership.OrgUnitID,
		"target_position_id": membership.PositionID, "change_type": identitydomain.PersonnelChangeTransfer,
		"status": identitydomain.PersonnelChangeScheduled, "reason": "隔离测试调度异动",
		"effective_at": time.Now().UTC().Add(time.Hour), "submitted_by": operatorID,
		"version": 1, "created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatalf("create scheduled personnel change fixture: %v", err)
	}
	if _, err := personnelChanges.Execute(ctx, application.PersonnelChangeRequest{
		ID: scheduledID, TenantID: tenantID, UserID: admin.UserID,
		ChangeType: identitydomain.PersonnelChangeTransfer, Status: identitydomain.PersonnelChangeScheduled, Version: 1,
	}, operatorID, time.Now().UTC()); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("Execute(personnel change) error = %v, want ErrProtected", err)
	}
	var scheduledStatus string
	if err := transaction.Table("iam_personnel_change_request").Select("status").Where("id = ?", scheduledID).Scan(&scheduledStatus).Error; err != nil {
		t.Fatalf("reload scheduled personnel change: %v", err)
	}
	if scheduledStatus != identitydomain.PersonnelChangeScheduled {
		t.Fatalf("scheduled personnel change status = %q, want %q", scheduledStatus, identitydomain.PersonnelChangeScheduled)
	}
	var membershipStatus string
	if err := transaction.Table("iam_membership").Select("status").Where("id = ?", membership.ID).Scan(&membershipStatus).Error; err != nil {
		t.Fatalf("reload protected membership: %v", err)
	}
	if membershipStatus != identitydomain.StatusActive {
		t.Fatalf("protected membership status = %q after rejected execution, want %q", membershipStatus, identitydomain.StatusActive)
	}

	binding, err := loadBootstrapBindingFixture(transaction, tenantID, admin.UserID)
	if err != nil {
		t.Fatalf("load bootstrap binding fixture: %v", err)
	}
	authorizationRepository, err := authorizationinfrastructure.NewGORMRepository(transaction)
	if err != nil {
		t.Fatalf("NewGORMRepository(authorization): %v", err)
	}
	operatorContext := authctx.WithPrincipal(ctx, authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: tenantID},
		User:    authctx.ReferenceName{ID: admin.UserID},
		Account: authctx.ReferenceName{ID: admin.AccountID},
	})
	if _, err := authorizationRepository.UpdateRoleBinding(operatorContext, tenantID, admin.UserID, domain.RoleBinding{
		ID: binding.ID, Role: domain.Reference{ID: binding.RoleID}, SubjectType: "USER",
		Subject: domain.Reference{ID: admin.UserID}, ScopeType: "TENANT", Status: domain.StatusDisabled, Version: binding.Version,
	}); !errors.Is(err, bootstrapadmin.ErrProtected) {
		t.Fatalf("UpdateRoleBinding(disable bootstrap grant) error = %v, want ErrProtected", err)
	}
	var preservedBinding struct {
		Status  string `gorm:"column:status"`
		Version uint64 `gorm:"column:version"`
	}
	if err := transaction.Table("authz_role_binding").Select("status, version").Where("id = ?", binding.ID).Take(&preservedBinding).Error; err != nil {
		t.Fatalf("reload bootstrap binding: %v", err)
	}
	if preservedBinding.Status != identitydomain.StatusActive || preservedBinding.Version != binding.Version {
		t.Fatalf("bootstrap binding changed after rejected update: %#v", preservedBinding)
	}

	var platformApplication struct {
		ID string `gorm:"column:id"`
	}
	if err := transaction.Table("platform_application").Select("id").
		Where("tenant_id = ? AND code = ?", tenantID, "platform").Take(&platformApplication).Error; err != nil {
		t.Fatalf("load platform application: %v", err)
	}
	ownerDirectoryRepository, err := ownerdirectoryinfrastructure.NewRepository(transaction)
	if err != nil {
		t.Fatalf("NewRepository(owner directory): %v", err)
	}
	ownerPage, err := ownerDirectoryRepository.List(ctx, tenantID, platformApplication.ID, "", ownerdirectoryapplication.Query{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("owner directory list: %v", err)
	}
	if ownerPage.Total != 0 || len(ownerPage.Items) != 0 {
		t.Fatalf("owner directory returned protected administrator: total=%d items=%#v", ownerPage.Total, ownerPage.Items)
	}
	exactPage, err := ownerDirectoryRepository.List(ctx, tenantID, platformApplication.ID, "", ownerdirectoryapplication.Query{UserID: admin.UserID, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("owner directory exact id list: %v", err)
	}
	if exactPage.Total != 0 || len(exactPage.Items) != 0 {
		t.Fatalf("owner directory exact id query bypassed hiding: total=%d items=%#v", exactPage.Total, exactPage.Items)
	}

	ordinaryUserID := "01JTESTORDINARY00000000001"
	now := time.Now().UTC()
	if err := transaction.Table("iam_user").Create(map[string]any{
		"id": ordinaryUserID, "tenant_id": tenantID, "display_name": "admin",
		"employment_status": "ACTIVE", "status": identitydomain.StatusActive,
		"version": 1, "created_at": now, "updated_at": now,
	}).Error; err != nil {
		t.Fatalf("create ordinary user fixture: %v", err)
	}
	personnelResolver, err := oidcpersonneldirectory.New(transaction)
	if err != nil {
		t.Fatalf("New(personnel directory): %v", err)
	}
	entries, err := personnelResolver.ListActivePersonnel(ctx, tenantID, "unused-oauth-client")
	if err != nil {
		t.Fatalf("personnel directory list: %v", err)
	}
	ordinaryFound := false
	for _, entry := range entries {
		if entry.UserID == admin.UserID {
			t.Fatalf("personnel directory returned protected administrator: %#v", entry)
		}
		if entry.UserID == ordinaryUserID {
			ordinaryFound = true
		}
	}
	if !ordinaryFound {
		t.Fatalf("personnel directory dropped the ordinary user: %#v", entries)
	}
	ordinary, err := repository.GetUser(ctx, tenantID, ordinaryUserID)
	if err != nil {
		t.Fatalf("GetUser(ordinary): %v", err)
	}
	if _, err := repository.UpdateUser(ctx, application.UserUpdateInput{
		TenantID: tenantID, OperatorID: operatorID, UserID: ordinaryUserID, DisplayName: "admin",
		Status: &disabled, Version: ordinary.Version,
	}, nil, nil); err != nil {
		t.Fatalf("UpdateUser(ordinary user named admin) error = %v, want ordinary management capability", err)
	}

	if _, initializedAgain, err := bootstrapadmin.Resolve(ctx, transaction, tenantID); err != nil || !initializedAgain {
		t.Fatalf("Resolve() after rejected writes = %v, %v; want intact initialization", initializedAgain, err)
	}

	// 已损坏的初始化事实只报告异常，不自动修复或改选其他管理员。
	if err := transaction.Table("iam_account").Where("id = ?", admin.AccountID).Update("status", disabled).Error; err != nil {
		t.Fatalf("damage bootstrap account fixture: %v", err)
	}
	if _, _, err := bootstrapadmin.Resolve(ctx, transaction, tenantID); !errors.Is(err, bootstrapadmin.ErrConfiguration) {
		t.Fatalf("Resolve() with disabled account error = %v, want ErrConfiguration", err)
	}
	var damagedStatus string
	if err := transaction.Table("iam_account").Select("status").Where("id = ?", admin.AccountID).Scan(&damagedStatus).Error; err != nil {
		t.Fatalf("reload damaged account: %v", err)
	}
	if damagedStatus != disabled {
		t.Fatalf("damaged account status = %q, want untouched %q", damagedStatus, disabled)
	}
	if err := transaction.Table("iam_bootstrap_state").Where("tenant_id = ?", tenantID).Delete(nil).Error; err != nil {
		t.Fatalf("remove bootstrap state fixture: %v", err)
	}
	if _, initialized, err := bootstrapadmin.Resolve(ctx, transaction, tenantID); initialized || !errors.Is(err, bootstrapadmin.ErrConfiguration) {
		t.Fatalf("Resolve() with missing initialization record = (%v, %v), want ErrConfiguration", initialized, err)
	}
}

type accountFixture struct {
	Version uint64 `gorm:"column:version"`
}

func loadAccountFixture(database *gorm.DB, tenantID, accountID string) (accountFixture, error) {
	var fixture accountFixture
	err := database.Table("iam_account").Select("version").Where("tenant_id = ? AND id = ?", tenantID, accountID).Take(&fixture).Error
	return fixture, err
}

type membershipFixture struct {
	ID              string `gorm:"column:id"`
	OrgUnitID       string `gorm:"column:org_unit_id"`
	PositionID      string `gorm:"column:position_id"`
	MembershipType  string `gorm:"column:membership_type"`
	Version         uint64 `gorm:"column:version"`
	PositionVersion uint64 `gorm:"column:position_version"`
	OrgUnitVersion  uint64 `gorm:"column:org_unit_version"`
}

func loadMembershipFixture(database *gorm.DB, tenantID, userID string) (membershipFixture, error) {
	var fixture membershipFixture
	err := database.Table("iam_membership AS membership").
		Select("membership.id, membership.org_unit_id, membership.position_id, membership.membership_type, membership.version, position.version AS position_version, organization.version AS org_unit_version").
		Joins("JOIN iam_position AS position ON position.id = membership.position_id AND position.tenant_id = membership.tenant_id").
		Joins("JOIN iam_org_unit AS organization ON organization.id = membership.org_unit_id AND organization.tenant_id = membership.tenant_id").
		Where("membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ?", tenantID, userID, identitydomain.StatusActive).
		Take(&fixture).Error
	return fixture, err
}

type bootstrapBindingFixture struct {
	ID      string `gorm:"column:id"`
	RoleID  string `gorm:"column:role_id"`
	Version uint64 `gorm:"column:version"`
}

func loadBootstrapBindingFixture(database *gorm.DB, tenantID, userID string) (bootstrapBindingFixture, error) {
	var fixture bootstrapBindingFixture
	err := database.Table("authz_role_binding AS binding").
		Select("binding.id, binding.role_id, binding.version").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id").
		Where("binding.tenant_id = ? AND binding.subject_type = ? AND binding.subject_id = ? AND role.code = ?", tenantID, "USER", userID, domain.SuperAdminRoleCode).
		Take(&fixture).Error
	return fixture, err
}
