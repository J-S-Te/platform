// Package applicationaccess 提供面向应用和 OAuth/OIDC 客户端的访问授权能力。
//
// 授权解析刻意从 OAuth Client 注册（而非硬编码子系统名）定位目标应用，使角色与
// 权限按应用隔离；应用自有目录（authorization catalog）声明角色-权限映射与最大
// 有效角色数策略，平台只存储和执行，不赋予权限码业务含义。
//
// 核心语义：
//   - 用户应用授权：仅管理 MANUAL 来源的用户直绑；组织/岗位模板与系统来源不受
//     用户编辑影响。删除为逻辑撤销（停用），继承角色仍出现在有效视图中。
//     管理响应 manual_roles 包含当前与未来生效、尚未过期的人工分配，供完整集合编辑；
//     roles/direct_roles/effective_permissions 仍只表达当前有效授权，不由管理集合推导。
//   - 主体（组织/岗位）应用授权：标准人事授权由岗位授权模板物化，手写入口恒拒绝；
//     历史绑定可查询与撤销。
//   - 授权目录：应用以 Bearer 凭据原子替换自有角色-权限清单；同版本同摘要视为幂等
//     确认；目录变更与授权 revision 推进、Keycloak 投影入队同事务提交。
//   - 令牌授权：OIDC 登录时按当前数据库快照解析有效角色/权限/组织 claims，并携带
//     目录哈希与修订号，供子系统识别目录变化或权限撤销。
//
// 平台自身的内置角色目录（PlatformApplicationCode）由 API 启动镜像而非 OAuth
// 发布流程维护，原因见 PlatformApplicationCode 常量注释。
package applicationaccess
