import { api } from '../api.js';

export async function getPublicBoards() {
    return await api('/boards/public');
}

export async function getPrivateBoards() {
    return await api('/boards/private');
}

export async function createBoard(boardData) {
    return await api('/boards', {
        method: 'POST',
        body: JSON.stringify(boardData)
    });
}

export async function deleteBoard(boardId) {
    return await api(`/boards/${boardId}`, {
        method: 'DELETE'
    });
}

export async function getBoardMembers(boardId) {
    return await api(`/boards/${boardId}/members`);
}

export async function removeMemberFromBoard(boardId, memberId) {
    return await api(`/boards/${boardId}/members/${memberId}`, {
        method: 'DELETE'
    });
}

export async function getInvitableUsers(boardId) {
    return await api(`/boards/${boardId}/invitable-users`);
}

export async function inviteUserToBoard(boardId, inviteeId) {
    return await api(`/boards/${boardId}/invite`, {
        method: 'POST',
        body: JSON.stringify({ invitee_id: inviteeId })
    });
}

export async function leaveBoard(boardId) {
  const token = await authService.getAccessToken();
  if (!token) return { ok: false, error: "Não autenticado" };
  try {
    const response = await fetch(`/api/boards/${boardId}/leave`, {
      method: "POST", 
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${token}`,
      },
    });
    if (!response.ok) return { ok: false, data: await response.json() };
    return { ok: true };
  } catch (error) {
    return { ok: false, error };
  }
}