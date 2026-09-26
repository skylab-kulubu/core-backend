-- A read link names the person its product acted for (on_behalf_of): a
-- current-identity link like any other (account-lifecycle.md), so it can name
-- only an active subject with no deletion marker. Rows written before a
-- person's erasure stay until their year is up (media.ReadLinkRetention):
-- they are the access audit record.
DROP TRIGGER IF EXISTS media_read_links_require_active_subject ON media_read_links;
CREATE TRIGGER media_read_links_require_active_subject
    BEFORE INSERT OR UPDATE OF on_behalf_of ON media_read_links
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('on_behalf_of');
