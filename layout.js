/* ==========================================================================
   机构合同管理系统 · 布局渲染（侧栏 + 顶栏）
   页面内调用 renderShell({ active, title, sub })
   ========================================================================== */

const ICONS = {
  dashboard: '<path d="M3 13h8V3H3v10zm0 8h8v-6H3v6zm10 0h8V11h-8v10zm0-18v6h8V3h-8z"/>',
  customers: '<path d="M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5c-1.66 0-3 1.34-3 3s1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5C6.34 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm8 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z"/>',
  contracts: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8l-6-6zm2 16H8v-2h8v2zm0-4H8v-2h8v2zm-3-5V3.5L18.5 9H13z"/>',
  templates: '<path d="M19 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V5a2 2 0 0 0-2-2zm0 16H5V5h14v14zM7 7h10v2H7V7zm0 4h10v2H7v-2zm0 4h7v2H7v-2z"/>',
  approvals: '<path d="M9 16.17 4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41L9 16.17z"/>',
  rules: '<path d="M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zM20.71 7.04a1 1 0 0 0 0-1.41l-2.34-2.34a1 1 0 0 0-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z"/>',
  sign: '<path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-2 15-5-5 1.41-1.41L10 14.17l7.59-7.59L19 8l-9 9z"/>',
  reports: '<path d="M5 9.2h3V19H5V9.2zM10.6 5h2.8v14h-2.8V5zm5.6 8H19v6h-2.8v-6z"/>',
  audit: '<path d="M12 1 3 5v6c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V5l-9-4zm-2 16-4-4 1.41-1.41L10 14.17l6.59-6.59L18 9l-8 8z"/>',
  settings: '<path d="M19.14 12.94c.04-.3.06-.61.06-.94 0-.32-.02-.64-.07-.94l2.03-1.58a.49.49 0 0 0 .12-.61l-1.92-3.32a.49.49 0 0 0-.59-.22l-2.39.96c-.5-.38-1.03-.7-1.62-.94l-.36-2.54a.48.48 0 0 0-.48-.41h-3.84a.48.48 0 0 0-.48.41l-.36 2.54c-.59.24-1.13.57-1.62.94l-2.39-.96a.48.48 0 0 0-.59.22L2.74 8.87a.48.48 0 0 0 .12.61l2.03 1.58c-.05.3-.07.62-.07.94 0 .32.02.64.07.94l-2.03 1.58a.49.49 0 0 0-.12.61l1.92 3.32c.13.22.39.31.59.22l2.39-.96c.5.38 1.03.7 1.62.94l.36 2.54c.05.24.24.41.48.41h3.84c.24 0 .44-.17.48-.41l.36-2.54c.59-.24 1.13-.56 1.62-.94l2.39.96c.22.09.47 0 .59-.22l1.92-3.32a.49.49 0 0 0-.12-.61l-2.03-1.58zM12 15.6a3.6 3.6 0 1 1 0-7.2 3.6 3.6 0 0 1 0 7.2z"/>',
  bell: '<path d="M12 22c1.1 0 2-.9 2-2h-4c0 1.1.9 2 2 2zm6-6v-5c0-3.07-1.64-5.64-4.5-6.32V4c0-.83-.67-1.5-1.5-1.5S10.5 3.17 10.5 4v.68C7.63 5.36 6 7.92 6 11v5l-2 2v1h16v-1l-2-2z"/>',
  search: '<path d="M15.5 14h-.79l-.28-.27a6.5 6.5 0 1 0-.7.7l.27.28v.79l5 4.99L20.49 19l-4.99-5zm-6 0A4.5 4.5 0 1 1 14 9.5 4.5 4.5 0 0 1 9.5 14z"/>',
};

const NAV = [
  { group: '业务' , items: [
    { key: 'dashboard',  label: '仪表盘',     icon: 'dashboard',  href: 'dashboard.html' },
    { key: 'customers',  label: '客户查询',   icon: 'customers',  href: '#' },
    { key: 'contracts',  label: '合同管理',   icon: 'contracts',  href: 'contracts.html' },
    { key: 'templates',  label: '合同模板',   icon: 'templates',  href: '#' },
  ]},
  { group: '流程', items: [
    { key: 'approvals',  label: '审批中心',   icon: 'approvals',  href: 'approvals.html', badge: 3 },
    { key: 'rules',      label: '审批规则',   icon: 'rules',      href: '#' },
    { key: 'sign',       label: '签署台账',   icon: 'sign',       href: '#' },
  ]},
  { group: '系统', items: [
    { key: 'reports',    label: '统计报表',   icon: 'reports',    href: '#' },
    { key: 'audit',      label: '审计日志',   icon: 'audit',      href: '#' },
    { key: 'settings',   label: '系统设置',   icon: 'settings',   href: '#' },
  ]},
];

function svg(path, cls) {
  return `<svg class="${cls || ''}" viewBox="0 0 24 24" fill="currentColor" width="18" height="18">${path}</svg>`;
}

function renderSidebar(active) {
  let nav = '';
  NAV.forEach(g => {
    nav += `<div class="nav-group-label">${g.group}</div>`;
    g.items.forEach(it => {
      const badge = it.badge ? `<span class="nav-badge">${it.badge}</span>` : '';
      nav += `<a class="nav-item ${it.key === active ? 'active' : ''}" href="${it.href}">
        <span class="nav-icon">${svg(ICONS[it.icon])}</span>
        <span>${it.label}</span>${badge}
      </a>`;
    });
  });
  return `
  <aside class="sidebar">
    <div class="sidebar-brand">
      <div class="logo">合</div>
      <div class="brand-text">
        <span class="brand-title">机构合同</span>
        <span class="brand-sub">Contract Manage</span>
      </div>
    </div>
    <nav class="sidebar-nav">${nav}</nav>
    <div class="sidebar-user">
      <div class="avatar">张</div>
      <div style="line-height:1.3">
        <div class="u-name">张伟</div>
        <div class="u-role">销售总监</div>
      </div>
    </div>
  </aside>`;
}

function renderTopbar(title, sub) {
  const crumb = sub
    ? `<span class="crumb-main">${title}</span><span class="crumb-sep">/</span><span class="crumb-sub">${sub}</span>`
    : `<span class="crumb-main">${title}</span>`;
  return `
  <header class="topbar">
    <div class="crumb">${crumb}</div>
    <div class="topbar-right">
      <div class="topbar-search">${svg(ICONS.search)}<span>搜索合同 / 客户…</span></div>
      <div class="icon-btn">${svg(ICONS.bell)}<span class="dot"></span></div>
      <div class="topbar-avatar">张</div>
    </div>
  </header>`;
}

function renderShell(opts) {
  const { active, title, sub } = opts;
  const sb = document.getElementById('sidebar-slot');
  const tb = document.getElementById('topbar-slot');
  if (sb) sb.outerHTML = renderSidebar(active);
  if (tb) tb.outerHTML = renderTopbar(title, sub);
}
