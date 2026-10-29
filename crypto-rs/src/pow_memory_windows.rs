use super::IdleMemory;
use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
use std::sync::{Arc, Condvar, Mutex};
use windows_sys::Win32::Foundation::{HANDLE, WAIT_OBJECT_0, WAIT_TIMEOUT};
use windows_sys::Win32::System::Memory::{
    CreateMemoryResourceNotification, HighMemoryResourceNotification, LowMemoryResourceNotification,
};
use windows_sys::Win32::System::Threading::{
    CreateEventW, SetEvent, WaitForMultipleObjects, WaitForSingleObject, INFINITE,
};

pub(super) struct Notifications {
    low: OwnedHandle,
    high: OwnedHandle,
    changed: OwnedHandle,
}

fn owned(raw: HANDLE) -> Option<OwnedHandle> {
    if raw.is_null() {
        None
    } else {
        // SAFETY: callers pass a newly created, uniquely owned handle.
        Some(unsafe { OwnedHandle::from_raw_handle(raw) })
    }
}

impl Notifications {
    pub(super) fn new() -> Option<Self> {
        // SAFETY: no pointer arguments; OwnedHandle closes partial creations if
        // a later operation fails. The wake event is private and auto-reset.
        unsafe {
            Some(Self {
                low: owned(CreateMemoryResourceNotification(
                    LowMemoryResourceNotification,
                ))?,
                high: owned(CreateMemoryResourceNotification(
                    HighMemoryResourceNotification,
                ))?,
                changed: owned(CreateEventW(std::ptr::null(), 0, 0, std::ptr::null()))?,
            })
        }
    }

    pub(super) fn wake(&self) {
        // SAFETY: this live, owned event allows the worker to observe cache
        // returns, policy changes and shutdown without polling.
        unsafe { SetEvent(self.changed.as_raw_handle()) };
    }

    fn under_pressure(&self) -> Result<bool, ()> {
        // SAFETY: a memory notification is waitable and remains alive while
        // this shared owner exists. A zero timeout only reads its state.
        match unsafe { WaitForSingleObject(self.low.as_raw_handle(), 0) } {
            WAIT_OBJECT_0 => Ok(true),
            WAIT_TIMEOUT => Ok(false),
            _ => Err(()),
        }
    }

    fn wait(&self, pressure: bool) -> Result<(), ()> {
        // While low memory stays signaled, wait for recovery or a cache change
        // instead of repeatedly waking on the same low-memory notification.
        let memory = if pressure { &self.high } else { &self.low };
        let handles = [self.changed.as_raw_handle(), memory.as_raw_handle()];
        // SAFETY: handles are distinct and owned for the entire wait. Pool drop
        // wakes and joins the worker before the final owner closes them.
        let result = unsafe { WaitForMultipleObjects(2, handles.as_ptr(), 0, INFINITE) };
        if result == WAIT_OBJECT_0 || result == WAIT_OBJECT_0 + 1 {
            Ok(())
        } else {
            Err(())
        }
    }
}

pub(super) fn reclaim_idle_memory(
    cache: Arc<Mutex<IdleMemory>>,
    wake: Arc<Condvar>,
    notifications: Arc<Notifications>,
) {
    let run = || -> Result<(), ()> {
        loop {
            let pressure = notifications.under_pressure()?;
            {
                let mut idle = cache.lock().map_err(|_| ())?;
                if idle.stopping {
                    return Ok(());
                }
                idle.pressure = pressure;
                if pressure {
                    // Only the idle arena is owned here. Active hashes retain
                    // their separate arenas and release them on return.
                    idle.memory = None;
                }
                idle.reclaim_at = None;
                wake.notify_all();
            }
            notifications.wait(pressure)?;
        }
    };
    if run().is_err() {
        // An unavailable notification service must not leave an unbounded idle
        // lifetime. Future returns use the synchronous release fallback.
        if let Ok(mut idle) = cache.lock() {
            idle.background = false;
            idle.memory = None;
            idle.reclaim_at = None;
        }
        wake.notify_all();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pow_memory::MemoryPool;
    use std::time::{Duration, Instant};
    use windows_sys::Win32::System::Threading::ResetEvent;

    fn controlled_notifications() -> Arc<Notifications> {
        // Manual-reset events reproduce the documented notification states
        // without exhausting the machine's RAM or changing system-wide state.
        unsafe {
            Arc::new(Notifications {
                low: owned(CreateEventW(std::ptr::null(), 1, 0, std::ptr::null())).unwrap(),
                high: owned(CreateEventW(std::ptr::null(), 1, 1, std::ptr::null())).unwrap(),
                changed: owned(CreateEventW(std::ptr::null(), 0, 0, std::ptr::null())).unwrap(),
            })
        }
    }

    fn wait_for(pool: &MemoryPool, predicate: impl Fn(&IdleMemory) -> bool) {
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut idle = pool.idle.lock().unwrap();
        while !predicate(&idle) {
            let remaining = deadline
                .checked_duration_since(Instant::now())
                .expect("worker timed out");
            idle = pool.wake.wait_timeout(idle, remaining).unwrap().0;
        }
    }

    #[test]
    fn pressure_releases_idle_memory_preserves_active_hashes_and_recovers() {
        let notifications = controlled_notifications();
        let pool = MemoryPool::new_inner(4096, Some(Arc::clone(&notifications)));
        let mut active = pool.checkout().unwrap();
        active.as_mut()[0].as_mut()[0] = 42;
        drop(pool.checkout().unwrap());
        wait_for(&pool, |idle| {
            idle.reclaim_at.is_none() && idle.memory.is_some()
        });
        unsafe {
            assert_ne!(ResetEvent(notifications.high.as_raw_handle()), 0);
            assert_ne!(SetEvent(notifications.low.as_raw_handle()), 0);
        }
        wait_for(&pool, |idle| idle.pressure && idle.memory.is_none());
        assert_eq!(active.as_mut()[0].as_ref()[0], 42);
        drop(active);
        drop(pool.checkout().unwrap());
        assert!(pool.idle.lock().unwrap().memory.is_none());
        unsafe {
            assert_ne!(ResetEvent(notifications.low.as_raw_handle()), 0);
            assert_ne!(SetEvent(notifications.high.as_raw_handle()), 0);
        }
        wait_for(&pool, |idle| !idle.pressure);
        drop(pool.checkout().unwrap());
        wait_for(&pool, |idle| {
            idle.reclaim_at.is_none() && idle.memory.is_some()
        });
        // Joining must wake a worker blocked on native wait handles.
        drop(pool);
    }

    #[test]
    fn missing_notifications_release_idle_memory() {
        let pool = MemoryPool::new_inner(32, None);
        assert!(pool.worker.is_none());
        drop(pool.checkout().unwrap());
        assert!(pool.idle.lock().unwrap().memory.is_none());
    }

    #[test]
    #[ignore = "allocates a production-size 2 GiB arena"]
    fn production_pressure_releases_committed_memory() {
        use windows_sys::Win32::System::ProcessStatus::{
            K32GetProcessMemoryInfo, PROCESS_MEMORY_COUNTERS_EX,
        };
        use windows_sys::Win32::System::Threading::GetCurrentProcess;
        fn memory() -> (usize, usize) {
            let mut counters: PROCESS_MEMORY_COUNTERS_EX = unsafe { std::mem::zeroed() };
            let size = std::mem::size_of_val(&counters) as u32;
            counters.cb = size;
            assert_ne!(
                unsafe {
                    K32GetProcessMemoryInfo(
                        GetCurrentProcess(),
                        (&mut counters as *mut PROCESS_MEMORY_COUNTERS_EX).cast(),
                        size,
                    )
                },
                0
            );
            (counters.WorkingSetSize, counters.PrivateUsage)
        }
        let notifications = controlled_notifications();
        let pool = MemoryPool::new_inner(2 * 1024 * 1024, Some(Arc::clone(&notifications)));
        drop(pool.checkout().unwrap());
        wait_for(&pool, |idle| {
            idle.reclaim_at.is_none() && idle.memory.is_some()
        });
        let before = memory();
        unsafe {
            assert_ne!(ResetEvent(notifications.high.as_raw_handle()), 0);
            assert_ne!(SetEvent(notifications.low.as_raw_handle()), 0);
        }
        wait_for(&pool, |idle| idle.pressure && idle.memory.is_none());
        let after = memory();
        println!(
            "before working_set={} commit={}; after working_set={} commit={}",
            before.0, before.1, after.0, after.1
        );
        assert!(before.1.saturating_sub(after.1) >= 2 * 1024 * 1024 * 1024 - 64 * 1024 * 1024);
    }
}
