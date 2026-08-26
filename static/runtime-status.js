(function (global) {
    'use strict';

    const statusMeta = Object.freeze({
        ok: { label: '正常', tone: 'ok' },
        running: { label: '执行中', tone: 'running' },
        waiting: { label: '等待中', tone: 'waiting' },
        disabled: { label: '未启用', tone: 'disabled' },
        error: { label: '异常', tone: 'error' }
    });

    const formatDateTime = (unix) => {
        const value = Number(unix || 0);
        if (!value) return '—';
        return new Intl.DateTimeFormat('zh-CN', {
            month: '2-digit',
            day: '2-digit',
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
            hour12: false
        }).format(new Date(value * 1000));
    };

    const formatDuration = (milliseconds) => {
        const value = Number(milliseconds || 0);
        if (value <= 0) return '—';
        if (value < 1000) return `${value} ms`;
        if (value < 60000) return `${(value / 1000).toFixed(value < 10000 ? 1 : 0)} 秒`;
        return `${Math.floor(value / 60000)} 分 ${Math.round((value % 60000) / 1000)} 秒`;
    };

    const metaFor = (status) => statusMeta[status] || { label: '未知', tone: 'disabled' };

    global.ECSRuntimeStatusUI = Object.freeze({ metaFor, formatDateTime, formatDuration });
})(window);
