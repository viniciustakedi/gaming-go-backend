-- DROP TABLE removes the table's own triggers as part of dropping the
-- table; the trigger function is a separate catalog object and is dropped
-- explicitly afterward, once nothing references it anymore.
DROP TABLE wager_transactions;
DROP FUNCTION wager_transactions_block_terminal_update();
