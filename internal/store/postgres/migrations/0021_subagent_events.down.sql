UPDATE events SET type = 'thread.' || substr(type, length('subagent.') + 1)
    WHERE type IN ('subagent.started', 'subagent.ended', 'subagent.message');

ALTER TABLE events RENAME COLUMN subagent TO thread;
