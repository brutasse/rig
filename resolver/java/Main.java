public class Main {
    public static void main(String[] args) throws Exception {
        String[] all = new String[args.length + 2];
        all[0] = "-m";
        all[1] = "rig.resolver.main";
        System.arraycopy(args, 0, all, 2, args.length);
        clojure.main.main(all);
    }
}
