-- Rolls back Forms' mail queue; what waits in it is not sent.
DROP TABLE IF EXISTS forms_mail_queue;
