const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || '/api/v1').replace(/\/$/, '')
export class NotificationError extends Error { constructor(message, options = {}) { super(message); this.name = 'NotificationError'; this.status = options.status || 0; this.code = options.code || '' } }
async function request(path, options = {}) { let response; try { response = await fetch(`${API_BASE_URL}${path}`, { credentials: 'include', headers: { Accept: 'application/json', ...(options.body ? { 'Content-Type': 'application/json' } : {}), ...(options.headers || {}) }, ...options }) } catch { throw new NotificationError('无法连接站内信服务。', { code: 'NETWORK_ERROR' }) }; const body = await response.json().catch(() => ({})); if (!response.ok) throw new NotificationError(body.message || '站内信请求失败。', { status: response.status, code: body.code }); return body.data }
export const listInbox = ({ page = 1, pageSize = 20 } = {}) => request(`/notifications/inbox?page=${page}&page_size=${pageSize}`)
export const getNotification = (deliveryID) => request(`/notifications/inbox/${encodeURIComponent(deliveryID)}`)
export const getUnreadCount = () => request('/notifications/inbox/unread-count')
export const markNotificationRead = (deliveryID) => request(`/notifications/inbox/${encodeURIComponent(deliveryID)}:read`, { method: 'POST', body: '{}' })
export const markAllNotificationsRead = () => request('/notifications/inbox:read-all', { method: 'POST', body: '{}' })
